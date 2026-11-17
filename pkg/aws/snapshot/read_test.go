package snapshot

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func resource(service, typ, arn, name string) model.Resource {
	return model.Resource{ARN: arn, Service: service, Type: typ, Account: account, Region: region, Name: name,
		Attributes: map[string]string{model.AttrID: name}}
}

// TestAssembleResolvesApplicationsInSourceOrder builds a stack, an
// AppRegistry application, tag sets and an alarm, then checks membership,
// merged tags, resolver order and derived flags.
func TestAssembleResolvesApplicationsInSourceOrder(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		sources := DefaultAppSources()
		store, _, _ := newStore(t, WithOptions(func() Options { return Options{AppSources: sources} }))
		ctx := context.Background()
		stackARN := "arn:aws:cloudformation:us-east-1:111122223333:stack/web/1"
		service := resource("ecs", "service", "arn:aws:ecs:us-east-1:111122223333:service/c/web", "web")
		function := resource("lambda", "function", "arn:aws:lambda:us-east-1:111122223333:function:worker", "worker")
		queue := resource("sqs", "queue", "arn:aws:sqs:us-east-1:111122223333:jobs", "jobs")
		queue.App, queue.AppSource = "from-own-tags", AppSourceTags

		stack := resource("cloudformation", "stack", stackARN, "web-stack")
		stack.Relations = []model.Relation{{Type: model.RelationAssociated, Target: service.ARN}, {Type: model.RelationAssociated, Target: "worker"}}
		application := resource(AppRegistryService, "application", "arn:aws:servicecatalog:us-east-1:111122223333:/applications/a", "shop")
		application.Relations = []model.Relation{{Type: model.RelationAssociated, Target: stackARN}}
		tags := model.Resource{ARN: function.ARN, Service: TagService, Type: "tags", Account: account, Region: region,
			Tags: map[string]string{"team": "jobs"}, App: "from-tags", AppSource: AppSourceTags}
		alarm := resource("cloudwatch", "alarm", "arn:aws:cloudwatch:us-east-1:111122223333:alarm:a", "a")
		alarm.Relations = []model.Relation{{Type: model.RelationMonitors, Target: service.ARN}}

		commits := map[string][]model.Resource{
			"ecs": {service}, "lambda": {function}, "sqs": {queue}, "cloudformation": {stack},
			AppRegistryService: {application}, TagService: {tags}, "cloudwatch": {alarm},
		}
		for name, resources := range commits {
			if _, err := store.Commit(unit(name), Candidate{Resources: resources}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		result, err := store.Resources(ctx, Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		byARN := map[string]model.Resource{}
		for _, item := range result.Resources {
			if item.Service == TagService {
				t.Fatal("tag set listed as a resource")
			}
			byARN[item.ARN] = item
		}
		if got := byARN[service.ARN]; got.App != "shop" || got.AppSource != AppSourceAppRegistry || got.Attributes[model.AttrStackName] != "web-stack" {
			t.Fatalf("service = %+v", got)
		}
		if got := byARN[function.ARN]; got.App != "shop" || got.Tags["team"] != "jobs" {
			t.Fatalf("function = %+v", got)
		}
		if got := byARN[queue.ARN]; got.App != "from-own-tags" || got.AppSource != AppSourceTags {
			t.Fatalf("queue = %+v", got)
		}

		// With tags first, the tagging API's application wins for the function.
		sources = []string{AppSourceTags, AppSourceCloudFormation}
		result, _ = store.Resources(ctx, Query{Account: account, ARN: function.ARN})
		if len(result.Resources) != 1 || result.Resources[0].App != "from-tags" {
			t.Fatalf("tags-first function = %+v", result.Resources)
		}
		sources = []string{AppSourceCloudFormation}
		result, _ = store.Resources(ctx, Query{Account: account, App: "web-stack"})
		if len(result.Resources) != 2 {
			t.Fatalf("stack-sourced application = %+v", result.Resources)
		}

		flags, _ := store.SecurityFlags(ctx, Query{Account: account})
		found := map[string]bool{}
		for _, flag := range flags.Flags {
			found[string(flag.Kind)+" "+flag.Resource] = true
		}
		if !found["no_alarms "+function.ARN] || found["no_alarms "+service.ARN] || !found["no_tags "+service.ARN] || found["no_tags "+function.ARN] {
			t.Fatalf("derived flags = %v", found)
		}

		single, status, ok, err := store.Resource(ctx, service.ARN, result.AsOf)
		if err != nil || !ok || single.ARN != service.ARN || status.Unit.Service != "ecs" {
			t.Fatalf("Resource = %+v %+v %v %v", single, status, ok, err)
		}
	})
}

// TestDerivedFlagsNeedTheUnitThatWouldDisproveThem: without a committed
// alarm or tagging unit there is no basis for "no alarms" or "no tags".
func TestDerivedFlagsNeedTheUnitThatWouldDisproveThem(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, _ := newStore(t)
		store.Commit(unit("ec2"), candidate("i-1"))
		flags, _ := store.SecurityFlags(context.Background(), Query{Account: account})
		if len(flags.Flags) != 0 {
			t.Fatalf("flags without evidence = %+v", flags.Flags)
		}
	})
}

func TestHealthFiltersByServiceAndApplication(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, _ := newStore(t)
		target := resource("elbv2", "target-group", "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/web/1", "web")
		target.App, target.AppSource = "shop", AppSourceTags
		health := model.Health{Resource: target.ARN, Account: account, Region: region, Source: model.SourceTargetHealth, Status: model.HealthWarning, Summary: "1 of 2 targets unhealthy", Ref: target.ARN}
		store.Commit(unit("elbv2"), Candidate{Resources: []model.Resource{target}, Health: []model.Health{health}})
		ctx := context.Background()
		for _, query := range []Query{{Service: "elbv2"}, {App: "shop"}, {ARN: target.ARN}, {Region: region}} {
			result, err := store.Health(ctx, query)
			if err != nil || len(result.Health) != 1 {
				t.Fatalf("query %+v = %+v, %v", query, result.Health, err)
			}
		}
		for _, query := range []Query{{Service: "ecs"}, {App: "other"}, {Region: "eu-west-1"}} {
			result, _ := store.Health(ctx, query)
			if len(result.Health) != 0 {
				t.Fatalf("query %+v matched %+v", query, result.Health)
			}
		}
		statuses, _ := store.Sync(ctx, Query{Account: account, Service: "elbv2"})
		if len(statuses) != 1 || statuses[0].Count != 1 {
			t.Fatalf("sync = %+v", statuses)
		}
	})
}
