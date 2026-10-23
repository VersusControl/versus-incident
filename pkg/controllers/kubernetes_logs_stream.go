package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/gofiber/fiber/v2"
)

func (controller *KubernetesAdminController) podLogStream(ctx *fiber.Ctx) error {
	options := kubernetes.PodLogStreamOptions{Namespace: strings.Clone(ctx.Params("namespace")), Pod: strings.Clone(ctx.Params("name")), Timestamps: true}
	arguments := ctx.Context().QueryArgs()
	var invalid bool
	arguments.VisitAll(func(key, value []byte) {
		name := string(key)
		if len(arguments.PeekMulti(name)) != 1 {
			invalid = true
			return
		}
		var err error
		switch name {
		case "cluster":
		case "container":
			options.Container = strings.Clone(string(value))
		case "cursor":
			options.Cursor = strings.Clone(string(value))
		case "since_seconds":
			options.SinceSeconds, err = strconv.Atoi(string(value))
			if options.SinceSeconds == 0 {
				invalid = true
			}
		case "tail_lines":
			options.TailLines, err = strconv.Atoi(string(value))
			if options.TailLines == 0 {
				invalid = true
			}
		case "previous":
			options.Previous, err = strconv.ParseBool(string(value))
		case "timestamps":
			options.Timestamps, err = strconv.ParseBool(string(value))
		default:
			invalid = true
		}
		if err != nil {
			invalid = true
		}
	})
	service := requestKubernetesService(ctx)
	if invalid {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	session, err := service.OpenPodLogStream(ctx.UserContext(), options)
	if err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	return serveKubernetesLogSSE(ctx, options.Cursor, func(streamContext context.Context, emit func(string, any) error) error {
		stop := context.AfterFunc(streamContext, session.Close)
		defer stop()
		return session.Stream(func(event kubernetes.PodLogStreamEvent) error {
			return emit(event.Event, event)
		})
	}, session.Close, nil)
}

type kubernetesSSEMessage struct {
	event string
	value any
}

// serveKubernetesLogSSE keeps one serialized, deadline-bounded writer for both
// Pod and workload reads. Disconnects cancel and join the producer.
func serveKubernetesLogSSE(ctx *fiber.Ctx, initialCursor string, run func(context.Context, func(string, any) error) error, closeSession func(), decorate func(kubernetes.PodLogStreamEvent) any) error {
	if decorate == nil {
		decorate = func(event kubernetes.PodLogStreamEvent) any { return event }
	}
	streamContext, cancel := context.WithCancel(ctx.UserContext())
	connection := ctx.Context().Conn()
	ctx.Set(fiber.HeaderContentType, "text/event-stream")
	ctx.Set(fiber.HeaderCacheControl, "no-store")
	ctx.Set("X-Accel-Buffering", "no")
	ctx.Set("X-Content-Type-Options", "nosniff")
	ctx.Context().SetBodyStreamWriter(func(writer *bufio.Writer) {
		defer cancel()
		if closeSession != nil {
			defer closeSession()
		}
		if connection != nil {
			defer connection.SetWriteDeadline(time.Time{})
		}
		lastCursor := initialCursor
		emit := func(event string, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if connection != nil {
				if err := connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return err
			}
			var checkpoint struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(encoded, &checkpoint)
			if checkpoint.Cursor != "" {
				lastCursor = checkpoint.Cursor
			}
			return nil
		}
		if err := emit("heartbeat", decorate(kubernetes.PodLogStreamEvent{Event: "heartbeat"})); err != nil {
			return
		}
		events := make(chan kubernetesSSEMessage)
		result := make(chan error, 1)
		workerDone := make(chan struct{})
		go func() {
			defer close(workerDone)
			result <- run(streamContext, func(event string, value any) error {
				select {
				case events <- kubernetesSSEMessage{event: event, value: value}:
					return nil
				case <-streamContext.Done():
					return streamContext.Err()
				}
			})
		}()
		defer func() { cancel(); <-workerDone }()
		probe := time.NewTicker(time.Second)
		defer probe.Stop()
		for {
			select {
			case <-streamContext.Done():
				return
			case event := <-events:
				if emit(event.event, event.value) != nil {
					return
				}
			case err := <-result:
				if err != nil && !errors.Is(err, context.Canceled) {
					detail := kubernetes.DiagnoseError(err)
					if emit("error", decorate(kubernetes.PodLogStreamEvent{Event: "error", Code: detail.Code, Message: detail.Message, Action: detail.Action, Retryable: detail.Retryable})) == nil {
						_ = emit("end", decorate(kubernetes.PodLogStreamEvent{Event: "end", Reason: "error", Cursor: lastCursor}))
					}
				}
				return
			case <-probe.C:
				if connection != nil {
					if connection.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
						return
					}
				}
				if _, err := writer.WriteString(": keepalive\n\n"); err != nil || writer.Flush() != nil {
					return
				}
			}
		}
	})
	return nil
}
