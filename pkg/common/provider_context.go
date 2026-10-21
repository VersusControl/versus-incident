package common

import (
	"context"
	"io"
	"net/http"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/slack-go/slack"
)

type deliveryTransport struct {
	context context.Context
	base    http.RoundTripper
}

func (t deliveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.context.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.context, cancel)
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return response, err
	}
	response.Body = &deliveryBody{ReadCloser: response.Body, cleanup: func() { stop(); cancel() }}
	return response, nil
}

type deliveryBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *deliveryBody) Close() error { defer b.cleanup(); return b.ReadCloser.Close() }
func deliveryClient(client *http.Client, ctx context.Context) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = deliveryTransport{ctx, base}
	return &copy
}
func bindProviderContext(provider core.AlertProvider, ctx context.Context) {
	switch p := provider.(type) {
	case *SlackProvider:
		p.client = slack.New(p.token, slack.OptionHTTPClient(deliveryClient(nil, ctx)))
	case *TelegramProvider:
		p.client = deliveryClient(p.client, ctx)
	case *ViberProvider:
		p.client = deliveryClient(p.client, ctx)
	case *MSTeamsProvider:
		p.client = deliveryClient(p.client, ctx)
	case *LarkProvider:
		p.client = deliveryClient(p.client, ctx)
	case *EmailProvider:
		p.context = ctx
	}
}
