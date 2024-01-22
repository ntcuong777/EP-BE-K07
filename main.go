package main

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"io"
	"log"
	"time"
)

const loginSessionDuration = 24 * time.Hour
const (
	pingApiSemaphoreKey   string = "__internal__pingApiSemaphore"
	pingApiSemaphoreCount int    = 1
	pingRateLimitCount    int    = 2
)

type resourcesResolver struct {
	redisClient *redis.Client
}

func main() {
	r := gin.Default()
	opt, err := redis.ParseURL("rediss://default:4cd481d1ef784d5f9eafc08f5e8c9e6c@apn1-obliging-gull-34428.upstash.io:34428")
	if err != nil {
		log.Fatal("Redis ParseURL error: ", err)
	}
	client := redis.NewClient(opt)

	resolver := &resourcesResolver{
		redisClient: client,
	}

	ctx := context.Background()
	client.Set(ctx, "foo", "bar", 0)
	val := client.Get(ctx, "foo").Val()
	print(val)

	r.POST("/login", loginHandler(resolver))
	r.GET("/ping", pingHandler(resolver))
	r.GET("/count", countHandler(resolver))
}

func loginHandler(r *resourcesResolver) gin.HandlerFunc {
	type userLogin struct {
		Username string `json:"username"`
		//Password string `json:"password"`
	}
	type userLoginResponse struct {
		SessionID string `json:"sessionId"`
	}
	return func(c *gin.Context) {
		var user userLogin
		if err := c.ShouldBindJSON(&user); err != nil {
			c.JSON(400, gin.H{
				"message": "Wrong login request format",
			})
			return
		}
		username := user.Username
		now := time.Now()
		expiredTime := now.Add(loginSessionDuration)
		// TODO: password validation later when database is added
		md5Hasher := md5.New()
		// simply use username, now, expiredTime to generate sessionID (for now)
		_, _ = io.WriteString(md5Hasher, fmt.Sprintf("%s-%s-%s", username, now.String(), expiredTime.String()))
		sessionID := fmt.Sprintf("%x", md5Hasher.Sum(nil))
		loginResp := userLoginResponse{
			SessionID: sessionID,
		}

		// Set login session to redis
		err := r.redisClient.Set(c, sessionID, username, loginSessionDuration).Err()
		if err != nil {
			c.JSON(500, gin.H{
				"message": "Internal server error",
			})
			return
		}

		c.JSON(200, loginResp)
	}
}

func lockPing(ctx context.Context, r *redis.Client) error {
	// Check if /ping is currently being called by another request
	cnt, err := r.Get(ctx, pingApiSemaphoreKey).Int()
	if err != nil {
		// If key is not found, set it to 0
		if errors.Is(err, redis.Nil) {
			err = r.Set(ctx, pingApiSemaphoreKey, 0, 0).Err()
			if err != nil {
				log.Println("redisClient.Set error: ", err)
				return fmt.Errorf("internal server error")
			}
			cnt = 0
		} else {
			log.Println("redisClient.Get error: ", err)
			return fmt.Errorf("internal server error")
		}
	}
	if cnt >= pingApiSemaphoreCount {
		return fmt.Errorf("too many requests")
	}
	// Increment semaphore count
	err = r.Incr(ctx, pingApiSemaphoreKey).Err()
	if err != nil {
		log.Println("redisClient.Incr error: ", err)
		return fmt.Errorf("internal server error")
	}
	return nil
}

func unlockPing(ctx context.Context, r *redis.Client, username string) error {
	// Decrement semaphore count
	err := r.Decr(ctx, pingApiSemaphoreKey).Err()
	if err != nil {
		log.Println("redisClient.Decr error: ", err)
		return fmt.Errorf("internal server error")
	}

	// Decrease rate limit count
	pingCountKey := fmt.Sprintf("__internal__pingCount-%s", username)
	err = r.Decr(ctx, pingCountKey).Err()
	if err != nil {
		log.Println("redisClient.Decr error: ", err)
		return fmt.Errorf("internal server error")
	}
	return nil
}

func pingRateLimit(ctx context.Context, r *redis.Client, username string) error {
	// count ping calls and limit to 2 calls per 60s/user
	pingCountKey := fmt.Sprintf("__internal__pingCount-%s", username)
	pingCount, err := r.Get(ctx, pingCountKey).Int()
	if err != nil {
		// If key is not found, set it to 0
		if errors.Is(err, redis.Nil) {
			err = r.Set(ctx, pingCountKey, 0, 60).Err()
			if err != nil {
				log.Println("redisClient.Set error: ", err)
				return fmt.Errorf("internal server error")
			}
			pingCount = 0
		} else {
			log.Println("redisClient.Get error: ", err)
			return fmt.Errorf("internal server error")
		}
	}
	if pingCount >= pingRateLimitCount {
		return fmt.Errorf("too many requests")
	}
	// Increment ping count
	err = r.Incr(ctx, pingCountKey).Err()
	if err != nil {
		log.Println("redisClient.Incr error: ", err)
		return fmt.Errorf("internal server error")
	}
	return nil
}

func pingHandler(r *resourcesResolver) gin.HandlerFunc {
	type pingRequest struct {
		SessionID string `json:"sessionId"`
	}
	return func(c *gin.Context) {
		// Check if sessionID is valid
		var pingReq pingRequest
		if err := c.ShouldBindJSON(&pingReq); err != nil {
			c.JSON(400, gin.H{
				"message": "Wrong ping request format",
			})
			return
		}
		sessionID := pingReq.SessionID
		username, err := r.redisClient.Get(c, sessionID).Result()
		if err != nil {
			c.JSON(401, gin.H{
				"message": "Unauthorized",
			})
			return
		}
		if username == "" {
			c.JSON(401, gin.H{
				"message": "Unauthorized",
			})
			return
		}

		// Lock ping API
		err = lockPing(c, r.redisClient)
		if err != nil {
			c.JSON(429, gin.H{
				"message": err.Error(),
			})
			return
		}
		defer func(ctx context.Context, r *redis.Client) {
			err := unlockPing(ctx, r, username)
			if err != nil {
				log.Println("unlockPing error: ", err)
			}
		}(c, r.redisClient)

		// count ping calls and limit to 2 calls per 60s/user
		err = pingRateLimit(c, r.redisClient, username)
		if err != nil {
			c.JSON(429, gin.H{
				"message": err.Error(),
			})
			return
		}

		// sleep for 5s
		time.Sleep(5 * time.Second)
		c.Status(200)
	}
}

func countHandler(r *resourcesResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	}
}
