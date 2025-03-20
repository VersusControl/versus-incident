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

func (w *OnCallWorkflow) Start(incidentID string, oc config.OnCallConfig) error {
	if oc.AwsIncidentManager.ResponsePlanArn == "" {
		return fmt.Errorf("missing Response Plan ARN configuration")
	}

	if w == nil || w.redisClient == nil {
		return fmt.Errorf("the on-call workflow hasn't been initiated yet")
	}

	ctx := context.Background()

	// To do, move this code into the factory or function
	// If WaitMinutes is 0, it means there’s no need to check for an acknowledgment, and the on-call will trigger immediately
	if oc.WaitMinutes == 0 {
		title := "Incident id " + incidentID
		input := &ssmincidents.StartIncidentInput{
			ResponsePlanArn: aws.String(oc.AwsIncidentManager.ResponsePlanArn),
			Title:           aws.String(title),
		}

		if _, err := w.client.StartIncident(ctx, input); err != nil {
			return fmt.Errorf("failed to start AWS incident: %v", err)
		}

		log.Printf("Incident escalated: %s", title)
		return nil
	}

	// If WaitMinutes isn't 0, store incident in Redis with expiration (wait time + buffer)
	expiration := time.Duration(oc.WaitMinutes)*time.Minute + 1*time.Minute
	if err := w.redisClient.Set(ctx, incidentID, "pending", expiration).Err(); err != nil {
		return fmt.Errorf("failed to store incident %s in Redis: %v", incidentID, err)
	}

	log.Printf("Incident On Call Workflow: %s", incidentID)

	// Start timer to check for acknowledgment
	go func() {
		<-time.After(time.Duration(oc.WaitMinutes) * time.Minute)

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
				ResponsePlanArn: aws.String(oc.AwsIncidentManager.ResponsePlanArn),
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

		return nil
	}

	return fmt.Errorf("incident does not exist or acknowledged")
}
