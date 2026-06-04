// Package email sends portfolio contact-form notifications via Amazon SES.
package email

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	ses "github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// SESAPI is the narrow SES interface required by SendContactEmail.
// The real *ses.Client satisfies it.
type SESAPI interface {
	SendEmail(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error)
}

// ContactEmailArgs holds the data needed to build and send the contact email.
type ContactEmailArgs struct {
	From      string
	To        string
	ReplyTo   string
	Name      string
	Email     string
	Message   string
	IP        string
	CreatedAt string
}

// SendContactEmail composes and dispatches a single SES email for a contact
// form submission. It mirrors the void TypeScript original: the SES output is
// discarded and only the error is returned.
func SendContactEmail(ctx context.Context, client SESAPI, a ContactEmailArgs) error {
	subject := "Portfolio: new contact from " + a.Name
	body := fmt.Sprintf("From: %s <%s>\nWhen: %s\nIP:   %s\n\n%s\n", a.Name, a.Email, a.CreatedAt, a.IP, a.Message)

	_, err := client.SendEmail(ctx, &ses.SendEmailInput{
		Source: aws.String(a.From),
		Destination: &types.Destination{
			ToAddresses: []string{a.To},
		},
		ReplyToAddresses: []string{a.ReplyTo},
		Message: &types.Message{
			Subject: &types.Content{
				Data: aws.String(subject),
			},
			Body: &types.Body{
				Text: &types.Content{
					Data: aws.String(body),
				},
			},
		},
	})
	return err
}
