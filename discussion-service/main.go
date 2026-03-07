package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hashicorp/consul/api"
	"github.com/golang-jwt/jwt/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	ginprometheus "github.com/zsais/go-gin-prometheus"
)

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
	db, err = gorm.Open(sqlite.Open("discussion.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	db.AutoMigrate(&Post{})
	fmt.Println("✅ Database connected and migrated!")

	// 📍 สั่งให้ Discussion ไปตั้งใจฟังประกาศจาก Moderation (Background)
	go consumeStatusUpdates("discussion_update_queue")

	r := gin.Default()

	p := ginprometheus.NewPrometheus("gin")
    p.Use(r)

	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	r.POST("/api/posts", createPost)
	r.GET("/api/posts", getPosts)
	r.GET("/api/posts/:id", getPostByID)
	r.PUT("/api/posts/:id", updatePost)
	r.DELETE("/api/posts/:id", deletePost)
	
	r.GET("/health", func(c *gin.Context) {
        c.JSON(http.StatusOK, gin.H{"status": "UP", "service": "discussion-service"})
    })

	registerWithConsul("discussion-service", 8080)

	fmt.Println("🚀 Discussion Service is running on port 8080...")
	r.Run(":8080")
}

func createPost(c *gin.Context) {
	// 📍 1. ตรวจสอบบัตรผ่าน (JWT) จาก Header
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ต้องล็อกอินก่อนสร้างกระทู้"})
		return
	}

	// รูปแบบคือ "Bearer <token>" เราต้องตัดคำว่า Bearer ออก
	tokenString := strings.TrimPrefix(authHeader, "Bearer ")
	
	// 📍 2. ถอดรหัสบัตรผ่าน (ต้องใช้ Secret Key เดียวกับ Auth Service)
	token, _ := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte("my_super_secret_key_100tip"), nil 
	})

	if token == nil || !token.Valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Session หมดอายุหรือบัตรผ่านไม่ถูกต้อง"})
		return
	}

	// 📍 3. ดึงชื่อ Username ออกมาจากบัตรผ่าน
	claims, _ := token.Claims.(jwt.MapClaims)
	username := claims["username"].(string)

	var newPost Post
	if err := c.ShouldBindJSON(&newPost); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ครบถ้วน"})
		return
	}
	
	newPost.Status = "pending"
	newPost.UserID = username // 👈 เปลี่ยนจาก "user_123" เป็นชื่อที่ดึงมาได้จาก Token จริงๆ!

	if err := db.Create(&newPost).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถบันทึกข้อมูลได้"})
		return
	}

	publishToRabbitMQ("moderation_queue", newPost)

	c.JSON(http.StatusCreated, gin.H{
		"message": "สร้างกระทู้สำเร็จ รอการตรวจสอบ",
		"post":    newPost,
	})
}

func getPosts(c *gin.Context) {
	var posts []Post
	db.Find(&posts)
	c.JSON(http.StatusOK, posts)
}

func getPostByID(c *gin.Context) {
	id := c.Param("id")
	var post Post
	if err := db.First(&post, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ไม่พบกระทู้"})
		return
	}
	c.JSON(http.StatusOK, post)
}

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

func deletePost(c *gin.Context) {
	id := c.Param("id")
	db.Delete(&Post{}, id)
	c.JSON(http.StatusOK, gin.H{"message": "ลบกระทู้สำเร็จ"})
}

// ==========================================
// RabbitMQ Functions
// ==========================================
func publishToRabbitMQ(queueName string, post Post) {
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Printf("⚠️ RabbitMQ Connection Error: %v", err)
		return
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

func consumeStatusUpdates(queueName string) {
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Println("⚠️ Consumer Failed to connect to RabbitMQ")
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	q, _ := ch.QueueDeclare(queueName, true, false, false, false, nil)
	msgs, _ := ch.Consume(q.Name, "", true, false, false, false, nil)

	log.Println("👂 Discussion Service is listening for status updates...")

	for d := range msgs {
		var updatePayload struct {
			PostID uint   `json:"post_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(d.Body, &updatePayload); err != nil {
			continue
		}

		var post Post
		if err := db.First(&post, updatePayload.PostID).Error; err == nil {
			post.Status = updatePayload.Status
			db.Save(&post)
			log.Printf("✅ Eventual Consistency Achieved! Post %d is now %s", post.ID, post.Status)
		}
	}
	
}
func registerWithConsul(serviceName string, port int) {
    config := api.DefaultConfig()
    config.Address = "consul:8500" // ชี้ไปที่ Container ของ Consul ใน Docker

    client, err := api.NewClient(config)
    if err != nil {
        log.Println("⚠️ ไม่สามารถเชื่อมต่อ Consul ได้:", err)
        return
    }

    registration := &api.AgentServiceRegistration{
        ID:      serviceName + "-1", // ตั้ง ID ให้ไม่ซ้ำกันเผื่อรันหลายตู้
        Name:    serviceName,
        Port:    port,
        Address: serviceName, // ใช้ชื่อ Container Name เป็น Address ให้ Docker หาเจอ
        Check: &api.AgentServiceCheck{
            HTTP:     fmt.Sprintf("http://%s:%d/health", serviceName, port),
            Interval: "10s", // ตรวจสุขภาพทุกๆ 10 วินาที
            Timeout:  "5s",
        },
    }

    err = client.Agent().ServiceRegister(registration)
    if err != nil {
        log.Printf("⚠️ รายงานตัวกับ Consul ไม่สำเร็จ: %v\n", err)
    } else {
        log.Printf("✅ %s รายงานตัวกับ Consul สำเร็จแล้ว!\n", serviceName)
    }
}