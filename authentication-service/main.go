package main

import (
	"fmt" 
	"log" 
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


var db *gorm.DB
var jwtSecret = []byte("my_super_secret_key_100tip") 

type User struct {
	gorm.Model
	Username string `gorm:"unique"`
	Password string
	Role     string `gorm:"default:'Member'"`
}

func main() {
	
	var err error
	db, err = gorm.Open(sqlite.Open("auth.db"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	db.AutoMigrate(&User{})

	
	r := gin.Default()
	p := ginprometheus.NewPrometheus("gin")
	p.Use(r)
	
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

	
	r.POST("/api/auth/register", register) 
	r.POST("/api/auth/login", login)       
	r.GET("/api/auth/users/:username/role", getUserRole)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "UP", "service": "authentication-service"})
	})

	
	registerWithConsul("authentication-service", 8082)

	
	r.Run(":8082")
}




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

	
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"exp":      time.Now().Add(time.Hour * 24).Unix(), 
	})

	tokenString, _ := token.SignedString(jwtSecret)

	c.JSON(http.StatusOK, gin.H{"token": tokenString})
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

	maxRetries := 10
		for i := 1; i <= maxRetries; i++ {
			err = client.Agent().ServiceRegister(registration)
			if err == nil {
				log.Printf("✅ %s รายงานตัวกับ Consul สำเร็จแล้ว!\n", serviceName)
				return 
			}
			
			log.Printf("⏳ Consul ยังไม่พร้อม (ลองครั้งที่ %d/%d) รอ 5 วินาที... Error: %v\n", i, maxRetries, err)
			time.Sleep(5 * time.Second) 
		}

		log.Printf("❌ ยอมแพ้! ไม่สามารถรายงานตัวกับ Consul ได้หลังจากพยายาม %d ครั้ง\n", maxRetries)
	}
func getUserRole(c *gin.Context) {
    username := c.Param("username")
    var user User
    
    
    if err := db.Where("username = ?", username).First(&user).Error; err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
        return
    }
    
    
    c.JSON(http.StatusOK, gin.H{"role": user.Role})
}
