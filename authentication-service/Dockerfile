FROM golang:1.25-alpine

# Install C Compiler (Crucial for SQLite to work in Auth Service)
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy files and download dependencies
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build the code
RUN go build -o main .

# Expose port 8081 (This is the port we set in your Auth main.go)
EXPOSE 8081

CMD ["./main"]