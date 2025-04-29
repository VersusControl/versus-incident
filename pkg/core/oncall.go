package core

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/aws/aws-sdk-go-v2/service/ssmincidents"
	"github.com/go-redis/redis/v8"
)

// OnCallProvider defines the interface for on-call notification providers
type OnCallProvider interface {
	TriggerOnCall(ctx context.Context, incidentID string) error
}

// Function that will be implemented in the common package to avoid circular imports
var CreateOnCallProvider func(cfg *config.Config, awsClient *ssmincidents.Client) (OnCallProvider, error)

// OnCallWorkflow coordinates on-call escalation with a single provider
type OnCallWorkflow struct {
	provider    OnCallProvider
	redisClient *redis.Client
}

// Global instance for singleton access
var (
	onCallWorkflow *OnCallWorkflow
	once           sync.Once
)

// NewOnCallWorkflow creates a new on-call workflow with the given provider
func NewOnCallWorkflow(redisClient *redis.Client, provider OnCallProvider) *OnCallWorkflow {
	return &OnCallWorkflow{
		provider:    provider,
		redisClient: redisClient,
	}
}

// InitOnCallWorkflow initializes the global singleton instance
// This is called once from main.go with the Redis client and AWS client
func InitOnCallWorkflow(awsClient *ssmincidents.Client, redisClient *redis.Client) {
	once.Do(func() {
		cfg := config.GetConfig()

		provider, err := CreateOnCallProvider(cfg, awsClient)
		if err != nil {
			log.Printf("Warning: Failed to create on-call provider: %v", err)
			provider = nil // No provider
		}

		onCallWorkflow = NewOnCallWorkflow(redisClient, provider)
		log.Println("On-call workflow initialized")
	})
}

// GetOnCallWorkflow returns the global singleton instance
// This maintains compatibility with existing code
func GetOnCallWorkflow() *OnCallWorkflow {
	if onCallWorkflow == nil {
		panic("on-call workflow not initialized - call InitOnCallWorkflow first")
	}
	return onCallWorkflow
}

// triggerProvider triggers the on-call provider
func (w *OnCallWorkflow) triggerProvider(ctx context.Context, incidentID string) error {
	if err := w.provider.TriggerOnCall(ctx, incidentID); err != nil {
		return err
	}
	return nil
}

// Start initiates the on-call workflow for an incident
func (w *OnCallWorkflow) Start(incidentID string, oc config.OnCallConfig) error {
	if w == nil || w.redisClient == nil {
		return fmt.Errorf("the on-call workflow hasn't been properly initialized")
	}

	if w.provider == nil {
		return fmt.Errorf("no on-call provider available")
	}

	ctx := context.Background()

	// If WaitMinutes is 0, trigger immediately
	if oc.WaitMinutes == 0 {
		return w.triggerProvider(ctx, incidentID)
	}

	// Store incident in Redis with expiration time
	expiration := time.Duration(oc.WaitMinutes)*time.Minute + 1*time.Minute
	if err := w.redisClient.Set(ctx, incidentID, "pending", expiration).Err(); err != nil {
		return fmt.Errorf("failed to store incident %s in Redis: %v", incidentID, err)
	}

	log.Printf("Incident %s queued with %d minute wait period", incidentID, oc.WaitMinutes)

	// Start timer to check for acknowledgment
	go func() {
		<-time.After(time.Duration(oc.WaitMinutes) * time.Minute)

		// Check if incident is still pending
		exists, err := w.redisClient.Exists(ctx, incidentID).Result()
		if err != nil {
			log.Printf("Failed to check incident %s in Redis: %v", incidentID, err)
			return
		}

		if exists == 1 {
			// If still pending, trigger on-call
			if err := w.triggerProvider(ctx, incidentID); err != nil {
				log.Printf("Failed to trigger provider: %v", err)
			}

			// Remove from Redis
			if err := w.redisClient.Del(ctx, incidentID).Err(); err != nil {
				log.Printf("Failed to delete incident %s from Redis: %v", incidentID, err)
			}
		}
	}()

	return nil
}

// Ack acknowledges an incident to prevent escalation
func (w *OnCallWorkflow) Ack(incidentID string) error {
	if w == nil || w.redisClient == nil {
		return fmt.Errorf("the on-call workflow hasn't been properly initialized")
	}

	// Delete incident from Redis to prevent escalation
	ctx := context.Background()
	exists, _ := w.redisClient.Exists(ctx, incidentID).Result()

	if exists == 1 {
		if err := w.redisClient.Del(ctx, incidentID).Err(); err != nil {
			return fmt.Errorf("failed to acknowledge incident %s: %v", incidentID, err)
		}
		return nil
	}

	return fmt.Errorf("incident does not exist or was already acknowledged")
}
