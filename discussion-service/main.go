package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hashicorp/consul/api"
	"github.com/golang-jwt/jwt/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"github.com/sony/gobreaker"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)
var roleBreaker *gobreaker.CircuitBreaker
var breakerGauge = promauto.NewGauge(prometheus.GaugeOpts{
    Name: "auth_breaker_state",
    Help: "State of the Auth Service Circuit Breaker (0:Closed, 1:Open, 2:HalfOpen)",
})
type Post struct {
	gorm.Model
	Title   string `json:"title" binding:"required"`
	Content string `json:"content" binding:"required"`
	UserID  string `json:"user_id"`
	Status  string `json:"status"` 
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
	
	roleBreaker = gobreaker.NewCircuitBreaker(gobreaker.Settings{
        Name:        "Auth-Role-Breaker",
        MaxRequests: 3,                
        Interval:    10 * time.Second,  
        Timeout:     40 * time.Second,  
        ReadyToTrip: func(counts gobreaker.Counts) bool {
            return counts.ConsecutiveFailures >= 3 
        },
        OnStateChange: func(name string, from, to gobreaker.State) {
            
            breakerGauge.Set(float64(to))
            log.Printf("🚨 เบรกเกอร์ [%s] เปลี่ยนสถานะ: %s -> %s", name, from, to)
        },
    })

	r.Run(":8080")
}

func createPost(c *gin.Context) {
	
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ต้องล็อกอินก่อนสร้างกระทู้"})
		return
	}

	
	tokenString := strings.TrimPrefix(authHeader, "Bearer ")
	
	
	token, _ := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte("my_super_secret_key_100tip"), nil 
	})

	if token == nil || !token.Valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Session หมดอายุหรือบัตรผ่านไม่ถูกต้อง"})
		return
	}

	
	claims, _ := token.Claims.(jwt.MapClaims)
	username := claims["username"].(string)

	var newPost Post
	if err := c.ShouldBindJSON(&newPost); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ครบถ้วน"})
		return
	}
	
	newPost.Status = "pending"
	newPost.UserID = username 

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
	userRole := fetchRoleWithBreaker(post.UserID)
	c.JSON(http.StatusOK, gin.H{
        "post": post,
        "author_role": userRole, 
    })
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
	var conn *amqp.Connection
	var err error

	
	for {
		conn, err = amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
		if err == nil {
			break 
		}
		log.Printf("⚠️ RabbitMQ ยังไม่พร้อม รอ 2 วินาที... (%v)", err)
		time.Sleep(2 * time.Second)
	}
	defer conn.Close()

	ch, err := conn.Channel()

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
    config.Address = "consul:8500" 

    client, err := api.NewClient(config)
    if err != nil {
        log.Println("⚠️ ไม่สามารถเชื่อมต่อ Consul ได้:", err)
        return
    }

    registration := &api.AgentServiceRegistration{
        ID:      serviceName + "-1", 
        Name:    serviceName,
        Port:    port,
        Address: serviceName, 
        Check: &api.AgentServiceCheck{
            HTTP:     fmt.Sprintf("http://%s:%d/health", serviceName, port),
            Interval: "10s", 
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
func fetchRoleWithBreaker(username string) string {
    result, err := roleBreaker.Execute(func() (interface{}, error) {
        
        resp, err := http.Get("http://authentication-service:8082/api/auth/users/" + username + "/role")
        if err != nil || resp.StatusCode != 200 {
            return nil, fmt.Errorf("Auth Service Error")
        }
        defer resp.Body.Close()
        
        
        var data struct { Role string `json:"role"` }
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
            return nil, err
        }
        return data.Role, nil
    })

  if err != nil {
        
        if err == gobreaker.ErrOpenState {
            log.Println("🚨 [Fast-Fail] เบรกเกอร์ทำงาน! เตะ Request ทิ้งทันทีไม่ต้องรอ!")
        } else {
            log.Println("⚠️ [Timeout] ติดต่อตู้ Auth ไม่ได้ (รอจนท้อแล้ว)")
        }
        return "Member (Offline)" 
    }
    return result.(string)
}