package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type ReviewTask struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	PostID  uint   `json:"post_id"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

var db *gorm.DB

func main() {
	var err error
	db, err = gorm.Open(sqlite.Open("moderation.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	db.AutoMigrate(&ReviewTask{})
	fmt.Println("✅ Moderation Database connected and migrated!")

	// สั่งให้ Worker ไปรอรับงานจาก RabbitMQ (Background)
	go consumeFromQueue("moderation_queue")

	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
	}))

	r.GET("/api/tasks", getReviewTasks)
	r.PATCH("/api/tasks/:id/status", updateTaskStatus)

	fmt.Println("🛡️ Moderation Service is running on port 8081...")
	r.Run(":8081")
}

func getReviewTasks(c *gin.Context) {
	var tasks []ReviewTask
	db.Find(&tasks)
	c.JSON(http.StatusOK, tasks)
}

func updateTaskStatus(c *gin.Context) {
	id := c.Param("id")
	var task ReviewTask
	
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ไม่พบตั๋วงาน"})
		return
	}

	var input struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ถูกต้อง"})
		return
	}

	task.Status = input.Status
	db.Save(&task)

	// 📍 ตะโกนบอก RabbitMQ เข้าคิวชื่อ "discussion_update_queue"
	publishStatusUpdate("discussion_update_queue", task.PostID, task.Status)

	c.JSON(http.StatusOK, gin.H{
		"message": "อัปเดตสถานะเรียบร้อย (Locally & Event Published)",
		"task":    task,
	})
}

// ==========================================
// RabbitMQ Functions
// ==========================================
func consumeFromQueue(queueName string) {
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Printf("⚠️ Failed to connect to RabbitMQ: %v", err)
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
	msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
	if err != nil {
		log.Printf("⚠️ Failed to register a consumer: %v", err)
		return
	}

	fmt.Println("🐰 Worker is waiting for posts...")

	for d := range msgs {
		var postPayload struct {
			ID      uint   `json:"ID"`
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		
		if err := json.Unmarshal(d.Body, &postPayload); err != nil {
			continue
		}

		newTask := ReviewTask{
			PostID:  postPayload.ID,
			Title:   postPayload.Title,
			Content: postPayload.Content,
			Status:  "pending",
		}
		db.Create(&newTask)
		log.Printf("📥 Received new Post (ID: %d) and saved as Review Task!", postPayload.ID)
	}
}

func publishStatusUpdate(queueName string, postID uint, status string) {
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	if err != nil {
		log.Printf("⚠️ RabbitMQ Error: %v", err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	q, _ := ch.QueueDeclare(queueName, true, false, false, false, nil)
	
	updateData := map[string]interface{}{
		"post_id": postID,
		"status":  status,
	}
	body, _ := json.Marshal(updateData)

	ch.Publish("", q.Name, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
	})
	log.Printf("📢 Published status update for Post %d to %s", postID, queueName)
}