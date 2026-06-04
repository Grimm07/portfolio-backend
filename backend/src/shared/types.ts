/** Raw JSON body posted by the contact form. */
export interface ContactSubmission {
  name: string;
  email: string;
  message: string;
  website: string;        // honeypot — must be empty
  formTimestamp: number;  // ms epoch when the form was rendered
  // (no turnstileToken — AWS WAF CAPTCHA validates aws-waf-token at the edge, not in this body)
}
