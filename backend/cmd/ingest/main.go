package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ses"

	"github.com/Grimm07/portfolio-backend/backend/internal/ingest"
)

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err) // cold-start infra error (no request PII) — acceptable to surface
	}

	deps := ingest.Deps{
		Env: ingest.Env{
			FromEmail:             os.Getenv("FROM_EMAIL"),
			ContactEmailSecretARN: os.Getenv("CONTACT_EMAIL_SECRET_ARN"),
			OriginVerifySecret:    os.Getenv("ORIGIN_VERIFY_SECRET"),
		},
		SES:     ses.NewFromConfig(cfg),
		Secrets: secretsmanager.NewFromConfig(cfg),
		Now:     time.Now,
	}

	lambda.Start(func(ctx context.Context, event events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return ingest.HandleIngest(ctx, event, deps)
	})
}
