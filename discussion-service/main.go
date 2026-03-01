package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ==========================================
// 1. Database Model
// ==========================================
type Post struct {
	gorm.Model
	Title   string `json:"title" binding:"required"`
	Content string `json:"content" binding:"required"`
	UserID  string `json:"user_id"`
	Status  string `json:"status"` // "pending", "approved", "rejected"
}

var db *gorm.DB

func main() {
	var err error
	// เชื่อมต่อ SQLite Database
	db, err = gorm.Open(sqlite.Open("discussion.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	db.AutoMigrate(&Post{})
	fmt.Println("✅ Database connected and migrated!")

	r := gin.Default()

	// ==========================================
	// 2. CORS Middleware (สำคัญมากสำหรับหน้าเว็บ)
	// ==========================================
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		// เปิดให้รองรับทุก Method รวมทั้ง PATCH สำหรับอัปเดตสถานะ
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// ==========================================
	// 3. API Routes (RESTful Standard)
	// ==========================================
	r.POST("/api/posts", createPost)             // สร้างกระทู้ใหม่
	r.GET("/api/posts", getPosts)                // ดึงกระทู้ทั้งหมด (Feed)
	r.GET("/api/posts/:id", getPostByID)         // ดึงข้อมูลกระทู้เดียว (หน้าอ่านรายละเอียด)
	r.PUT("/api/posts/:id", updatePost)          // แก้ไขกระทู้
	r.DELETE("/api/posts/:id", deletePost)       // ลบกระทู้
	r.PATCH("/api/posts/:id/status", updatePostStatus) // แอดมินอัปเดตสถานะ

	fmt.Println("🚀 Discussion Service is running on port 8080...")
	r.Run(":8080")
}

// ==========================================
// Controllers (ฟังก์ชันจัดการ API)
// ==========================================

// สร้างกระทู้ใหม่
func createPost(c *gin.Context) {
	var newPost Post
	if err := c.ShouldBindJSON(&newPost); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ครบถ้วน"})
		return
	}

	newPost.Status = "pending"  // เริ่มต้นเป็นรอตรวจสอบ
	newPost.UserID = "user_123" // (Mock) ในอนาคตดึงจาก Auth Token

	if err := db.Create(&newPost).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถบันทึกข้อมูลได้"})
		return
	}

	// 📍 ส่ง Event เข้า RabbitMQ เพื่อให้ Moderation ไปตรวจ
	publishToRabbitMQ("moderation_queue", newPost)

	c.JSON(http.StatusCreated, gin.H{
		"message": "สร้างกระทู้สำเร็จ รอการตรวจสอบ",
		"post":    newPost,
	})
}

// ดึงกระทู้ทั้งหมด
func getPosts(c *gin.Context) {
	var posts []Post
	db.Find(&posts)
	c.JSON(http.StatusOK, posts)
}

// ดึงเฉพาะกระทู้เดียว (จาก ID)
func getPostByID(c *gin.Context) {
	id := c.Param("id")
	var post Post
	if err := db.First(&post, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ไม่พบกระทู้"})
		return
	}
	c.JSON(http.StatusOK, post)
}

// แก้ไขกระทู้
func updatePost(c *gin.Context) {
	id := c.Param("id")
	var post Post
	if err := db.First(&post, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ไม่พบกระทู้"})
		return
	}

	var updateData Post
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ถูกต้อง"})
		return
	}

	post.Title = updateData.Title
	post.Content = updateData.Content
	db.Save(&post)
	c.JSON(http.StatusOK, post)
}

// ลบกระทู้
func deletePost(c *gin.Context) {
	id := c.Param("id")
	db.Delete(&Post{}, id)
	c.JSON(http.StatusOK, gin.H{"message": "ลบกระทู้สำเร็จ"})
}

// แอดมินแก้ไขสถานะ
func updatePostStatus(c *gin.Context) {
	id := c.Param("id")
	var post Post
	if err := db.First(&post, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ไม่พบกระทู้"})
		return
	}

	var statusUpdate struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&statusUpdate); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ถูกต้อง"})
		return
	}

	post.Status = statusUpdate.Status
	db.Save(&post)
	c.JSON(http.StatusOK, gin.H{"message": "อัปเดตสถานะเป็น " + post.Status})
}

// ==========================================
// RabbitMQ Publisher
// ==========================================

func publishToRabbitMQ(queueName string, post Post) {
	// ใช้ host "rabbitmq" สำหรับรันใน Docker
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Printf("⚠️ RabbitMQ Connection Error: %v", err)
		return // ระบบยังทำงานต่อได้ แม้ RabbitMQ จะล่ม (Fault Tolerance)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Printf("⚠️ Failed to open channel: %v", err)
		return
	}
	defer ch.Close()

	q, _ := ch.QueueDeclare(queueName, true, false, false, false, nil)
	body, _ := json.Marshal(post)

	err = ch.Publish("", q.Name, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
	})
	if err == nil {
		log.Printf("🐰 [x] Sent Post ID %d to RabbitMQ (Queue: %s)", post.ID, queueName)
	}
}