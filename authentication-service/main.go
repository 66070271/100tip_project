package main

import (
	"fmt" // 👈 เพิ่มบรรทัดนี้
	"log" // 👈 เพิ่มบรรทัดนี้
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/hashicorp/consul/api"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 1. Setup the Database and Secret Key
var db *gorm.DB
var jwtSecret = []byte("my_super_secret_key_100tip") // In a real app, hide this!

type User struct {
	gorm.Model
	Username string `gorm:"unique"`
	Password string
	Role     string `gorm:"default:'Member'"`
}

func main() {
	// Connect to SQLite (This will create an auth.db file automatically)
	var err error
	db, err = gorm.Open(sqlite.Open("auth.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	db.AutoMigrate(&User{}) // Creates the user table

	// 2. Setup the Web Server
	r := gin.Default()
	p := ginprometheus.NewPrometheus("gin")
	p.Use(r)
	// CORS Middleware to let your HTML files talk to this API
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// 3. Our Two API Endpoints
	r.POST("/api/auth/register", register) // เปลี่ยนจาก /register
	r.POST("/api/auth/login", login)       // เปลี่ยนจาก /login
	r.GET("/api/auth/users/:username/role", getUserRole)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "UP", "service": "authentication-service"})
	})

	// 📍 2. รายงานตัวชื่อ authentication-service พอร์ต 8082
	registerWithConsul("authentication-service", 8082)

	// Run on port 8081 (Assuming Discussion service runs on 8080)
	r.Run(":8082")
}

// --- Handler Functions ---

// Register: Takes username/password, hashes password, saves to DB
func register(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid input"})
		return
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(input.Password), 14)
	user := User{Username: input.Username, Password: string(hashedPassword)}

	if err := db.Create(&user).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Username already exists"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "User registered successfully!"})
}

// Login: Checks credentials, gives out a JWT wristband
func login(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid input"})
		return
	}

	var user User
	if err := db.Where("username = ?", input.Username).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Wrong password"})
		return
	}

	// Create the JWT token (The Bouncer's Wristband)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"exp":      time.Now().Add(time.Hour * 24).Unix(), // Expires in 24 hours
	})

	tokenString, _ := token.SignedString(jwtSecret)

	c.JSON(http.StatusOK, gin.H{"token": tokenString})
}

// ==========================================
// Consul Registration Function
// ==========================================
func registerWithConsul(serviceName string, port int) {
	config := api.DefaultConfig()
	config.Address = "consul:8500" // ชี้ไปที่ Container ของ Consul

	client, err := api.NewClient(config)
	if err != nil {
		log.Println("⚠️ ไม่สามารถเชื่อมต่อ Consul ได้:", err)
		return
	}

	registration := &api.AgentServiceRegistration{
		ID:      serviceName + "-1",
		Name:    serviceName,
		Port:    port,
		Address: serviceName, // ให้ Docker หาเจอผ่านชื่อ Container
		Check: &api.AgentServiceCheck{
			HTTP:     fmt.Sprintf("http://%s:%d/health", serviceName, port),
			Interval: "10s",
			Timeout:  "5s",
		},
	}

	maxRetries := 10
		for i := 1; i <= maxRetries; i++ {
			err = client.Agent().ServiceRegister(registration)
			if err == nil {
				log.Printf("✅ %s รายงานตัวกับ Consul สำเร็จแล้ว!\n", serviceName)
				return // ถ้าสำเร็จก็จบฟังก์ชันเลย ไม่ต้องทำต่อ
			}
			
			log.Printf("⏳ Consul ยังไม่พร้อม (ลองครั้งที่ %d/%d) รอ 5 วินาที... Error: %v\n", i, maxRetries, err)
			time.Sleep(5 * time.Second) // รอ 5 วินาทีก่อนเคาะประตูใหม่
		}

		log.Printf("❌ ยอมแพ้! ไม่สามารถรายงานตัวกับ Consul ได้หลังจากพยายาม %d ครั้ง\n", maxRetries)
	}
func getUserRole(c *gin.Context) {
    username := c.Param("username")
    var user User
    
    // ค้นหา User จากชื่อ
    if err := db.Where("username = ?", username).First(&user).Error; err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
        return
    }
    
    // ส่งยศกลับไปให้
    c.JSON(http.StatusOK, gin.H{"role": user.Role})
}
