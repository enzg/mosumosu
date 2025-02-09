package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	// 改为 segmentio/kafka-go
	"github.com/joho/godotenv"
	"github.com/segmentio/kafka-go"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/gofiber/fiber/v2"

	// 假设你还有 GORM + MySQL
	"mosumosu.com/kafkaFiber/db"

	_ "github.com/go-sql-driver/mysql"
)

// ================ 主入口 ================
func main() {
	// 先加载 .env
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatalf("Error loading .env file")
	}
	// 1) 连接 MySQL
	// user:password@tcp(host:port)/database ...
	// read from .env
	// Load environment variables
	dsn := fmt.Sprintf("%s:%s@tcp(127.0.0.1:13306)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASS"),
		os.Getenv("DB_NAME"))
	fmt.Println(dsn)
	db.ConnectMySQL(dsn) // 内部会 autoMigrate & 赋值 db.DB

	// 2) 连接 Elasticsearch
	es, err := elasticsearch.NewDefaultClient()
	if err != nil {
		log.Fatalf("[FATAL] create ES client error: %v", err)
	}
	if resp, err := es.Info(); err != nil {
		log.Printf("[WARN] es.Info() failed: %v", err)
	} else {
		resp.Body.Close()
		log.Println("[INFO] Elasticsearch connected.")
	}

	// 3) 启动 Fiber
	app := fiber.New()
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	// 4) 启动 Kafka Consumer (kafka-go)
	go runKafkaConsumer(es)

	// 5) Fiber Listen
	log.Fatal(app.Listen(":3000"))
}

// ================ 启动消费者 ================
func runKafkaConsumer(es *elasticsearch.Client) {
	// 你在 docker-compose 或本地 Broker: "localhost:9092"
	// 这里设置 GroupID: "pixiv-group", 订阅 Topic: "crawler-pixiv"
	// 若有多 broker, 可写 []string{"host1:9092", "host2:9092"}
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{"localhost:9092"},
		GroupID:  "pixiv-group",
		Topic:    "crawler-pixiv",
		MinBytes: 1,    // 每次最少读1字节
		MaxBytes: 10e6, // 每次最多读10MB
	})
	defer r.Close()

	// 建立一个 context，用于优雅退出
	ctx := context.Background()

	log.Println("[INFO] Kafka reader started. Waiting for messages...")

	for {
		// ReadMessage 会阻塞直到读到一条消息或发生错误
		m, err := r.ReadMessage(ctx)
		if err != nil {
			// 若是 context 被取消 或其他不可恢复错误，可退出循环
			log.Printf("[ERROR] r.ReadMessage error: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		rawJSON := string(m.Value)
		log.Printf(">>> [Kafka] Partition=%d Offset=%d Key=%s msg_len=%d",
			m.Partition, m.Offset, string(m.Key), len(rawJSON))

		// 同步处理消息
		if e := handlePixivJSON(rawJSON, es); e != nil {
			log.Printf("[ERROR] handlePixivJSON: %v", e)
		}
		// kafka-go 在使用 GroupID 时，会自动把 offset 提交到 consumer group coordinator
		// 不需要手动 session.MarkMessage(...)
	}
}

// ================ 消费者处理逻辑 ================

// handlePixivJSON 解析 Pixiv preload-data 并插入 MySQL、写入 ES
func handlePixivJSON(jsonStr string, es *elasticsearch.Client) error {
	// 先整体解析成 map
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &root); err != nil {
		return fmt.Errorf("json.Unmarshal error: %w", err)
	}

	// novel 下取第一个key
	novelObj, ok := root["novel"].(map[string]interface{})
	if !ok || len(novelObj) == 0 {
		return fmt.Errorf("no novel found in the JSON")
	}

	var firstNovel map[string]interface{}
	for _, v := range novelObj {
		firstNovel, _ = v.(map[string]interface{})
		break
	}
	if firstNovel == nil {
		return fmt.Errorf("first novel is nil")
	}

	// 构建 db.Article
	article := db.Article{
		Title:        getString(firstNovel["title"]),
		Author:       getString(firstNovel["userName"]),
		Platform:     "Pixiv",
		Summary:      getString(firstNovel["description"]),
		Content:      getString(firstNovel["content"]),
		WordCount:    getInt(firstNovel["wordCount"]),
		IsCompleted:  false,
		KudosCount:   getInt(firstNovel["likeCount"]),
		CommentCount: getInt(firstNovel["commentCount"]),
		Language:     getString(firstNovel["language"]),
	}

	// 处理 tags
	if tagsMap, ok := firstNovel["tags"].(map[string]interface{}); ok {
		if tagsArr, ok := tagsMap["tags"].([]interface{}); ok {
			if tagsBytes, err := json.Marshal(tagsArr); err == nil {
				article.Tags = string(tagsBytes)
			}
		}
	}

	// 用 GORM 插入 MySQL
	if err := db.DB.Create(&article).Error; err != nil {
		return fmt.Errorf("insertArticle: %w", err)
	}
	log.Printf("[INFO] Inserted article '%s' into MySQL, ID=%d", article.Title, article.ID)

	// 写入 ES
	if err := indexToElasticsearch(es, firstNovel); err != nil {
		return fmt.Errorf("indexToElasticsearch: %w", err)
	}

	return nil
}

// 写 ES
func indexToElasticsearch(es *elasticsearch.Client, novel map[string]interface{}) error {
	// 构造
	doc := map[string]interface{}{
		"article_id": getString(novel["id"]),
		"title":      getString(novel["title"]),
		"author": map[string]interface{}{
			"name":        getString(novel["userName"]),
			"profile_url": fmt.Sprintf("https://www.pixiv.net/users/%v", getString(novel["userId"])),
			"status":      "active",
		},
		"cover_image":         getString(novel["coverUrl"]),
		"content":             getString(novel["content"]),
		"word_count":          getInt(novel["wordCount"]),
		"language":            getString(novel["language"]),
		"status":              "completed",
		"chapters":            []interface{}{},
		"likes":               getInt(novel["likeCount"]),
		"comments_count":      getInt(novel["commentCount"]),
		"comments":            []interface{}{},
		"tags":                extractTagList(novel),
		"published_at":        getString(novel["uploadDate"]),
		"estimated_read_time": fmt.Sprintf("%d分", getInt(novel["readingTime"])/60),
		"interaction": map[string]interface{}{
			"reaction_count": 0,
			"likes_count":    getInt(novel["likeCount"]),
			"views_count":    getInt(novel["viewCount"]),
		},
	}

	// 序列化
	bodyBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}

	idxResp, err := es.Index(
		"es_article",
		strings.NewReader(string(bodyBytes)),
		es.Index.WithRefresh("true"),
	)
	if err != nil {
		return err
	}
	defer idxResp.Body.Close()
	if idxResp.IsError() {
		return fmt.Errorf("ES index error: %s", idxResp.String())
	}
	return nil
}

// 提取标签
func extractTagList(novel map[string]interface{}) []string {
	var tagList []string
	if tagsObj, ok := novel["tags"].(map[string]interface{}); ok {
		if tagsArr, ok := tagsObj["tags"].([]interface{}); ok {
			for _, t := range tagsArr {
				if eachTag, ok := t.(map[string]interface{}); ok {
					tagList = append(tagList, getString(eachTag["tag"]))
				}
			}
		}
	}
	return tagList
}

// 辅助函数
func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func getInt(v interface{}) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int32:
		return int(val)
	case int64:
		return int(val)
	default:
		return 0
	}
}
