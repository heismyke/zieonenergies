package app

import (
	"bytes"
	"html/template"
	"strings"
)

// Customer emails: a dark header with the logo, a white card with a greeting,
// a bold headline, a soft details panel and a short "why you got this" note.
// Table layout and inline styles only, since Gmail and Outlook drop most CSS;
// the one <style> rule only tightens spacing on phones.

// tokenEmail is the data for the token receipt email.
type tokenEmail struct {
	Brand, BrandName, Logo, Accent string
	Preheader, Headline, Hello     string
	Sandbox                        bool
	Token                          string // grouped; empty for postpaid
	Kwh                            string // "79.9"
	Meter, Disco, Name             string
	Date, Ref, Paid                string
	Electricity, Fee               string // Fee is empty when there is none
	Total                          string
	ReceiptURL, SiteURL            string
	SiteHost, Support              string
	TermsURL, PrivacyURL           string
}

// resetEmail is the data for the password reset email.
type resetEmail struct {
	Brand, BrandName, Logo, Accent string
	Hello, Email, Link             string
	Sandbox                        bool // always false; the shared header reads it
	Support, SiteURL, SiteHost     string
}

const emailHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light"><meta name="supported-color-schemes" content="light"><title>{{.Preheader}}</title>
<style>@media (max-width:480px){.pad{padding-left:22px!important;padding-right:22px!important}.big{font-size:23px!important}.tok{font-size:19px!important;letter-spacing:0!important}}</style></head>
<body style="margin:0;padding:0;background:#f3f4f6;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:#f3f4f6;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f3f4f6;">
<tr><td align="center" style="padding:28px 10px 40px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:580px;font-family:Helvetica,Arial,sans-serif;color:#111827;">
<tr><td class="pad" style="background:#0b0b0b;border-radius:14px 14px 0 0;padding:26px 40px;">
  <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
    {{if .Logo}}<td style="padding-right:12px;"><img src="{{.Logo}}" width="40" height="40" alt="" style="display:block;border-radius:10px;border:0;"></td>{{end}}
    <td style="font-size:24px;font-weight:bold;letter-spacing:-0.5px;color:#ffffff;">{{.BrandName}}</td>
  </tr></table>
</td></tr>
{{if .Sandbox}}<tr><td class="pad" style="background:#fff4e5;padding:12px 40px;font-size:13px;color:#8a4b00;"><b>Test purchase.</b> This token will not work on a real meter.</td></tr>{{end}}
<tr><td style="background:#ffffff;border-radius:0 0 14px 14px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">`

const emailFoot = `  </table>
</td></tr>
<tr><td align="center" style="padding:22px 16px 0;font-size:12px;line-height:1.7;color:#9ca3af;">
  {{.Footer}}
</td></tr>
</table>
</td></tr></table>
</body></html>`

// detailRow is one line of the details panel.
const detailRow = `<tr><td style="padding:16px 0;border-bottom:1px solid #dde3ec;font-weight:bold;color:#111827;">%s</td><td align="right" style="padding:16px 0 16px 12px;border-bottom:1px solid #dde3ec;color:#374151;">%s</td></tr>`

func row(label, value string) string {
	return strings.Replace(strings.Replace(detailRow, "%s", label, 1), "%s", value, 1)
}

const lastRow = `<tr><td style="padding:16px 0 4px;font-weight:bold;color:#111827;">%s</td><td align="right" style="padding:16px 0 4px 12px;font-size:17px;font-weight:bold;color:#111827;">%s</td></tr>`

func last(label, value string) string {
	return strings.Replace(strings.Replace(lastRow, "%s", label, 1), "%s", value, 1)
}

// button is a dark call-to-action; a table cell so Outlook keeps the padding.
func button(href, text string) string {
	return `<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td style="background:#0b0b0b;border-radius:8px;">
      <a href="` + href + `" style="display:inline-block;padding:14px 32px;font-size:15px;font-weight:bold;color:#ffffff;text-decoration:none;">` + text + `</a></td></tr></table>`
}

const emailWhy = `
  <tr><td class="pad" style="padding:32px 40px 36px;font-size:14px;line-height:1.65;color:#4b5563;">
    <p style="margin:0 0 8px;font-weight:bold;color:#111827;">Why did I receive this email?</p>
    <p style="margin:0;">{{.Why}}</p>
    <p style="margin:12px 0 0;">Need help? {{if .Support}}Reply to this email or write to <a href="mailto:{{.Support}}" style="color:#111827;">{{.Support}}</a>.{{else}}Reply to this email.{{end}}</p>
    <p style="margin:20px 0 0;color:#111827;">&mdash; {{.BrandName}}</p>
  </td></tr>
`

var tokenEmailTmpl = template.Must(template.New("token").Parse(emailHead + `
  <tr><td class="pad" style="padding:34px 40px 0;">
    <p style="margin:0;font-size:16px;color:#374151;">{{.Hello}}</p>
    <p class="big" style="margin:14px 0 0;font-size:27px;line-height:1.25;font-weight:bold;color:#111827;">{{.Headline}}</p>
    <p style="margin:12px 0 0;font-size:16px;line-height:1.55;color:#374151;">{{if .Token}}Here is your {{.Disco}} token for meter <b>{{.Meter}}</b>.{{else}}Your {{.Disco}} bill payment for meter <b>{{.Meter}}</b> was received.{{end}}</p>
  </td></tr>
  {{if .Token}}<tr><td class="pad" style="padding:22px 40px 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#0b0b0b;border-radius:12px;">
      <tr><td align="center" style="padding:24px 16px 22px;">
        <p style="margin:0;font-size:12px;font-weight:bold;letter-spacing:2px;color:#9ca3af;">YOUR TOKEN</p>
        <p class="tok" style="margin:10px 0 0;white-space:nowrap;font-family:'Courier New',Courier,monospace;font-size:28px;line-height:1.2;font-weight:bold;letter-spacing:1px;color:{{.Accent}};">{{.Token}}</p>
        <p style="margin:10px 0 0;font-size:14px;color:#d1d5db;">{{.Kwh}} kWh</p>
      </td></tr>
    </table>
    <p style="margin:12px 0 0;font-size:14px;line-height:1.5;color:#6b7280;">Type the 20 digits on your meter keypad, then press <b>Enter</b>.</p>
  </td></tr>{{end}}
  <tr><td class="pad" style="padding:28px 40px 0;">
    <p style="margin:0 0 14px;font-size:16px;color:#374151;">The details are shown below:</p>
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f1f4f9;border-radius:12px;">
      <tr><td style="padding:6px 26px 14px;">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:15px;">
          ` + row("Electricity company", "{{.Disco}}") + `
          ` + row("Meter number", "{{.Meter}}") + `
          {{if .Name}}` + row("Account name", "{{.Name}}") + `{{end}}
          {{if .Token}}` + row("Units", "{{.Kwh}} kWh") + `{{end}}
          ` + row("Amount", "{{.Electricity}}") + `
          {{if .Fee}}` + row("Service fee", "{{.Fee}}") + `{{end}}
          ` + row("Paid with", "{{.Paid}}") + `
          ` + row("Reference", "{{.Ref}}") + `
          ` + row("Date &amp; time", "{{.Date}}") + `
          ` + last("Total paid", "{{.Total}}") + `
        </table>
      </td></tr>
    </table>
  </td></tr>
  {{if .ReceiptURL}}<tr><td class="pad" style="padding:28px 40px 0;">` + button("{{.ReceiptURL}}", "View receipt") + `</td></tr>{{end}}` + emailWhy + emailFoot))

var resetEmailTmpl = template.Must(template.New("reset").Parse(emailHead + `
  <tr><td class="pad" style="padding:34px 40px 0;">
    <p style="margin:0;font-size:16px;color:#374151;">{{.Hello}}</p>
    <p class="big" style="margin:14px 0 0;font-size:27px;line-height:1.25;font-weight:bold;color:#111827;">Reset your password</p>
    <p style="margin:12px 0 0;font-size:16px;line-height:1.55;color:#374151;">We got a request to reset the password for <b>{{.Email}}</b>. Click the button below to choose a new one.</p>
  </td></tr>
  <tr><td class="pad" style="padding:26px 40px 0;">` + button("{{.Link}}", "Reset password") + `
    <p style="margin:12px 0 0;font-size:13px;color:#6b7280;">The link works once and expires in 30 minutes.</p>
  </td></tr>
  <tr><td class="pad" style="padding:24px 40px 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f1f4f9;border-radius:12px;">
      <tr><td style="padding:18px 22px;font-size:13px;line-height:1.6;color:#4b5563;">Button not working? Paste this link into your browser:<br><a href="{{.Link}}" style="color:#374151;word-break:break-all;">{{.Link}}</a></td></tr>
    </table>
  </td></tr>` + emailWhy + emailFoot))

// footerLinks is the site link (and Terms/Privacy) under the email.
func footerLinks(site, terms, privacy string) template.HTML {
	if site == "" {
		return ""
	}
	a := func(href, text string) string {
		return `<a href="` + template.HTMLEscapeString(href) + `" style="color:#9ca3af;text-decoration:none;">` + template.HTMLEscapeString(text) + `</a>`
	}
	out := a(site, siteHost(site))
	if terms != "" {
		out += ` &nbsp;&middot;&nbsp; ` + a(terms, "Terms") + ` &nbsp;&middot;&nbsp; ` + a(privacy, "Privacy")
	}
	return template.HTML(out)
}

func renderTokenEmail(e tokenEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = siteHost(e.SiteURL)
	why := "You bought electricity on " + e.BrandName + " with this email address. Keep this email: it has your token and receipt."
	return execEmail(tokenEmailTmpl, struct {
		tokenEmail
		Why    string
		Footer template.HTML
	}{e, why, footerLinks(e.SiteURL, e.TermsURL, e.PrivacyURL)})
}

func renderResetEmail(e resetEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = siteHost(e.SiteURL)
	return execEmail(resetEmailTmpl, struct {
		resetEmail
		Preheader, Why string
		Footer         template.HTML
	}{e, "Set a new password for " + e.Email + ". The link expires in 30 minutes.",
		"Someone, hopefully you, asked to reset the password for this " + e.BrandName + " account. If it wasn't you, ignore this email and your password stays the same.",
		footerLinks(e.SiteURL, "", "")})
}

func siteHost(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
}

func execEmail(t *template.Template, data any) string {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return ""
	}
	return b.String()
}
