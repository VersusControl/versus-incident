package core

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssmincidents"
	"github.com/go-redis/redis/v8"
)

type OnCallWorkflow struct {
	client      *ssmincidents.Client // AWS Incident Manager client
	redisClient *redis.Client        // Redis client
}

var (
	workflow *OnCallWorkflow
	once     sync.Once
)

func InitOnCallWorkflow(
	awsClient *ssmincidents.Client,
	redisClient *redis.Client,
) {
	once.Do(func() {
		workflow = &OnCallWorkflow{
			client:      awsClient,
			redisClient: redisClient,
		}
	})
}

func GetOnCallWorkflow() *OnCallWorkflow {
	return workflow
}

func (w *OnCallWorkflow) Start(incidentID string, imc config.AwsIncidentManagerConfig) error {
	if imc.ResponsePlanARN == "" {
		return fmt.Errorf("missing Response Plan ARN configuration")
	}

	if w == nil || w.redisClient == nil {
		return fmt.Errorf("the on-call workflow hasn't been initiated yet")
	}

	ctx := context.Background()

	// Store incident in Redis with expiration (wait time + buffer)
	expiration := time.Duration(imc.WaitMinutes)*time.Minute + 1*time.Minute
	if err := w.redisClient.Set(ctx, incidentID, "pending", expiration).Err(); err != nil {
		return fmt.Errorf("failed to store incident %s in Redis: %v", incidentID, err)
	}

	// Start timer to check for acknowledgment
	go func() {
		<-time.After(time.Duration(imc.WaitMinutes) * time.Minute)

		// Check if the incident is still pending
		exists, err := w.redisClient.Exists(ctx, incidentID).Result()
		if err != nil {
			log.Printf("Failed to check incident %s in Redis: %v", incidentID, err)
			return
		}

		if exists == 1 {
			// Trigger AWS Incident Manager
			title := "Incident id " + incidentID
			input := &ssmincidents.StartIncidentInput{
				ResponsePlanArn: aws.String(imc.ResponsePlanARN),
				Title:           aws.String(title),
			}

			if _, err := w.client.StartIncident(ctx, input); err != nil {
				log.Printf("Failed to start AWS incident: %v", err)
			} else {
				log.Printf("Incident escalated: %s", title)
			}

			// Cleanup
			if err := w.redisClient.Del(ctx, incidentID).Err(); err != nil {
				log.Printf("Failed to delete incident %s: %v", incidentID, err)
			}
		}
	}()

	return nil
}

func (w *OnCallWorkflow) Ack(incidentID string) error {
	if w == nil || w.redisClient == nil {
		return fmt.Errorf("the on-call workflow hasn't been initiated yet")
	}

	// Delete incident from Redis on acknowledgment
	ctx := context.Background()

	// Check if the key exists
	exists, _ := w.redisClient.Exists(ctx, incidentID).Result()

	if exists == 1 {
		if err := w.redisClient.Del(ctx, incidentID).Err(); err != nil {
			return fmt.Errorf("failed to acknowledge incident %s: %v", incidentID, err)
		}
	}

	return fmt.Errorf("incident does not exist or acknowledged")
}
