# อัปเดตเป็น Go 1.25
FROM golang:1.25-alpine

# ติดตั้ง C Compiler สำหรับ SQLite
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# ก๊อปปี้ไฟล์โค้ดและไลบรารี
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build โค้ดเป็นไฟล์ชื่อ main
RUN go build -o main .

# เปิดพอร์ต 8081 สำหรับ Moderation
EXPOSE 8081

CMD ["./main"]