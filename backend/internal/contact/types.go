// Package contact defines the request shape for the portfolio contact form.
package contact

// Submission is the raw JSON body posted by the contact form.
//
// Website is a honeypot field and must be empty on a legitimate submission.
// FormTimestamp is the millisecond-epoch time at which the form was rendered,
// used to reject submissions that arrive suspiciously fast.
type Submission struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	Message       string `json:"message"`
	Website       string `json:"website"`       // honeypot — must be empty
	FormTimestamp int64  `json:"formTimestamp"` // ms epoch when the form was rendered
}
