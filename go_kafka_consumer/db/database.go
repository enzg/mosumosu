package db

import (
	"log"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// DB 全局变量，提供给其他地方使用
var DB *gorm.DB

// Article 结构体对应 MySQL 中的表 "article"。
// 你可以在这里加上更多 GORM tag 做更精细的映射。
type Article struct {
	ID           uint   `gorm:"primaryKey;autoIncrement"`
	Title        string `gorm:"type:varchar(500);not null"`
	Author       string `gorm:"type:varchar(255)"`
	Platform     string `gorm:"type:enum('ao3','Pixiv','lofter','weibo')"`
	Summary      string `gorm:"type:text"`
	Content      string `gorm:"type:longtext"`
	WordCount    int
	IsCompleted  bool   `gorm:"default:false"`
	KudosCount   int    `gorm:"default:0"`
	CommentCount int    `gorm:"default:0"`
	Language     string `gorm:"type:varchar(50)"`
	// tags 可能用 JSON 类型（MySQL8+），这里简化用 text
	Tags string `gorm:"type:text"`

	// 如果需要 created_at/updated_at，可以这样：
	// CreatedAt time.Time
	// UpdatedAt time.Time
}

// ConnectMySQL 使用 GORM 连接 MySQL，并自动迁移 article 表
func ConnectMySQL(dsn string) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{
			SingularTable: true, // 不要自动加复数
		},
	})
	if err != nil {
		log.Fatalf("[FATAL] gorm.Open error: %v", err)
	}

	// 赋值给全局 DB
	DB = db

	// 自动建表/更新表结构
	if err := DB.AutoMigrate(&Article{}); err != nil {
		log.Fatalf("[FATAL] AutoMigrate error: %v", err)
	}

	log.Println("[INFO] MySQL connected & AutoMigrate done via GORM.")
}
