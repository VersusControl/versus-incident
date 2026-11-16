package kubernetes

import (
	"context"
	"net/url"
)

// defaultContainerAnnotation is the annotation kubectl reads to choose the
// container a log or exec command targets when none is named.
const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// injectedSidecarContainers names containers a service mesh or secrets
// injector adds next to the application. A log read that names no container
// skips them, because a reader asking for "the Pod's logs" means the
// application, not its proxy.
var injectedSidecarContainers = map[string]struct{}{
	"istio-proxy":      {},
	"linkerd-proxy":    {},
	"vault-agent":      {},
	"consul-dataplane": {},
}

type logContainerName struct {
	Name string `json:"name"`
}

// logContainerPod is the part of a Pod a default log container is chosen from.
type logContainerPod struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Containers []logContainerName `json:"containers"`
	} `json:"spec"`
}

// preferredLogContainer is the one rule for the container a log view shows
// when the reader names none, in the order kubectl logs uses: the
// default-container annotation when it names one of the regular containers,
// then the first regular container that is not an injected sidecar, then the
// first regular container. The live read, the Pod summary and the workload
// stream all choose through it so they never disagree.
func preferredLogContainer(annotation string, containers []string) string {
	if len(containers) == 0 {
		return ""
	}
	if annotation != "" {
		for _, name := range containers {
			if name == annotation {
				return name
			}
		}
	}
	for _, name := range containers {
		if _, sidecar := injectedSidecarContainers[name]; name != "" && !sidecar {
			return name
		}
	}
	return containers[0]
}

// defaultLogContainer picks the container a container-less log read of the Pod
// is retried against. A Pod with one regular container returns "": Kubernetes
// already picks that container itself.
func defaultLogContainer(pod logContainerPod) string {
	if len(pod.Spec.Containers) < 2 {
		return ""
	}
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}
	return preferredLogContainer(pod.Metadata.Annotations[defaultContainerAnnotation], names)
}

// resolveLogContainer chooses the container for a log read that named none and
// was refused because the Pod has several. The Pod is read with the same client
// and credentials as the log read, so no other permission is used. When the Pod
// cannot be read, or it has a single container, the result is empty and the
// caller keeps the refusal it already has.
func (service *Service) resolveLogContainer(ctx context.Context, namespace, pod string) string {
	if service == nil || service.client == nil {
		return ""
	}
	var object logContainerPod
	if err := service.client.GetJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/"+url.PathEscape(pod), &object); err != nil {
		return ""
	}
	resolved := defaultLogContainer(object)
	if resolved != "" && !validPodLogName(resolved, 63, false) {
		return ""
	}
	return resolved
}
