package controllers

import (
	"context"
	"strconv"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/gofiber/fiber/v2"
)

func (controller *KubernetesAdminController) usageHistory(ctx *fiber.Ctx) error {
	arguments := ctx.Context().QueryArgs()
	invalid := false
	arguments.VisitAll(func(key, value []byte) {
		name := string(key)
		if len(arguments.PeekMulti(name)) != 1 || name != "cluster" && name != "namespace" && name != "pod" {
			invalid = true
		}
	})
	if invalid {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	result, err := requestKubernetesService(ctx).UsageHistory(ctx.UserContext(), ctx.Query("namespace"), ctx.Query("pod"))
	return writeKubernetes(ctx, result, err)
}

func (controller *KubernetesAdminController) workloadLogStream(ctx *fiber.Ctx) error {
	options := kubernetes.WorkloadLogStreamOptions{Kind: strings.Clone(ctx.Params("kind")), Name: strings.Clone(ctx.Params("name")), Timestamps: true}
	arguments := ctx.Context().QueryArgs()
	invalid := false
	arguments.VisitAll(func(key, value []byte) {
		name := string(key)
		if name != "pod_uid" && len(arguments.PeekMulti(name)) != 1 {
			invalid = true
		}
		var err error
		switch name {
		case "cluster":
		case "namespace":
			options.Namespace = strings.Clone(string(value))
		case "uid":
			options.UID = strings.Clone(string(value))
		case "pod_uid":
			options.PodUIDs = append(options.PodUIDs, strings.Clone(string(value)))
		case "container":
			options.Container = strings.Clone(string(value))
		case "cursor":
			options.Cursor = strings.Clone(string(value))
		case "previous":
			options.Previous, err = strconv.ParseBool(string(value))
		case "timestamps":
			options.Timestamps, err = strconv.ParseBool(string(value))
		case "since_seconds":
			options.SinceSeconds, err = strconv.Atoi(string(value))
			invalid = invalid || options.SinceSeconds == 0
		case "tail_lines":
			options.TailLines, err = strconv.Atoi(string(value))
			invalid = invalid || options.TailLines == 0
		default:
			invalid = true
		}
		invalid = invalid || err != nil
	})
	if invalid {
		return writeKubernetes(ctx, nil, kubernetes.ErrInvalidArguments)
	}
	// Reserve the viewer and initial sources before committing SSE headers.
	// The service context already carries Enterprise revocation when installed.
	session, err := requestKubernetesService(ctx).OpenWorkloadLogStream(ctx.UserContext(), options)
	if err != nil {
		return writeKubernetes(ctx, nil, err)
	}
	return serveKubernetesLogSSE(ctx, options.Cursor, func(streamContext context.Context, emit func(string, any) error) error {
		stop := context.AfterFunc(streamContext, session.Close)
		defer stop()
		return session.Stream(func(event kubernetes.WorkloadLogStreamEvent) error {
			return emit(event.Event, event)
		})
	}, session.Close, func(event kubernetes.PodLogStreamEvent) any {
		return kubernetes.WorkloadLogStreamEvent{PodLogStreamEvent: event, WorkloadUID: options.UID, Namespace: options.Namespace}
	})
}
