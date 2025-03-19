package core

type QueueListener interface {
	StartListening(handler func(content *map[string]interface{}) error) error
}
