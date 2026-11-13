package app

import (
	"bytes"
	"html/template"
	"strings"
)

// Customer emails: a light page with the logo on top, a dark brand panel for
// the headline, a details table and a short footer. Table layout and inline
// styles only, since Gmail and Outlook drop most CSS; the one <style> rule
// only tightens spacing on phones.

// tokenEmail is the data for the token receipt email.
type tokenEmail struct {
	Brand, BrandName, Logo, Accent string
	Preheader, Headline            string
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
	Support, SiteURL, SiteHost     string
}

const emailHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light"><meta name="supported-color-schemes" content="light"><title>{{.Preheader}}</title>
<style>@media (max-width:480px){.pad{padding-left:20px!important;padding-right:20px!important}.big{font-size:24px!important}.tok{font-size:19px!important;letter-spacing:0!important}}</style></head>
<body style="margin:0;padding:0;background:#f3f2ef;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:#f3f2ef;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f3f2ef;">
<tr><td align="center" style="padding:32px 12px 40px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;font-family:Helvetica,Arial,sans-serif;color:#1a1a1a;">
<tr><td align="center" style="padding:0 16px 22px;">
  {{if .Logo}}<img src="{{.Logo}}" width="44" height="44" alt="{{.BrandName}}" style="display:block;border-radius:11px;border:0;">{{else}}<span style="font-size:16px;font-weight:bold;letter-spacing:4px;">{{.Brand}}</span>{{end}}
  <p style="margin:14px 0 0;font-size:13px;line-height:1.5;color:#6b6b6b;">{{.Intro}}</p>
</td></tr>
<tr><td style="background:#ffffff;border:1px solid #e6e4df;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">`

const emailFoot = `  </table>
</td></tr>
<tr><td align="center" style="padding:24px 16px 0;font-size:12px;line-height:1.7;color:#8a8a8a;">
  {{.Footer}}
</td></tr>
</table>
</td></tr></table>
</body></html>`

const emailSignoff = `
  <tr><td align="center" style="padding:24px 32px 30px;"><div style="width:40px;border-top:1px solid #dddddd;font-size:0;line-height:0;">&nbsp;</div>
    <p style="margin:18px 0 0;font-size:15px;color:#444444;">{{.BrandName}}</p>
    {{if .Support}}<p style="margin:4px 0 0;font-size:14px;"><a href="mailto:{{.Support}}" style="color:{{.Accent}};">{{.Support}}</a></p>{{end}}
  </td></tr>
`

const detailRow = `<tr><td style="padding:13px 0;border-bottom:1px solid #eeeeee;color:#6b6b6b;">%s</td><td align="right" style="padding:13px 0;border-bottom:1px solid #eeeeee;font-weight:bold;color:#333333;">%s</td></tr>`

func row(label, value string) string {
	return strings.Replace(strings.Replace(detailRow, "%s", label, 1), "%s", value, 1)
}

var tokenEmailTmpl = template.Must(template.New("token").Parse(emailHead + `
  {{if .Sandbox}}<tr><td align="center" style="background:#d9534f;padding:13px 20px;font-size:13px;color:#ffffff;">Test purchase. This token will not work on a real meter.</td></tr>{{end}}
  <tr><td class="pad" align="center" style="background:#111111;padding:34px 32px 32px;">
    <p style="margin:0;font-size:15px;line-height:1.5;color:#d6d6d6;">{{if .Token}}Your {{.Disco}} token{{else}}Your {{.Disco}} bill payment of{{end}}</p>
    {{if .Token}}
    <p class="tok" style="margin:14px 0 0;white-space:nowrap;font-family:'Courier New',Courier,monospace;font-size:30px;line-height:1.2;font-weight:bold;letter-spacing:1px;color:{{.Accent}};">{{.Token}}</p>
    <p style="margin:12px 0 0;font-size:14px;color:#9a9a9a;">{{.Kwh}} kWh &middot; meter {{.Meter}}</p>
    {{else}}
    <p class="big" style="margin:12px 0 0;font-size:34px;font-weight:bold;color:#ffffff;">{{.Electricity}}</p>
    <p style="margin:10px 0 0;font-size:14px;color:#9a9a9a;">was received for meter {{.Meter}}</p>
    {{end}}
  </td></tr>
  {{if .Token}}<tr><td class="pad" align="center" style="padding:16px 32px;background:#faf6f0;border-bottom:1px solid #efe9df;font-size:13px;color:#6b5a45;">Type the 20 digits on your meter keypad, then press <b>Enter</b>.</td></tr>{{end}}
  <tr><td class="pad" style="padding:30px 32px 8px;">
    <p style="margin:0 0 14px;font-size:17px;font-weight:bold;color:#333333;" align="center">Transaction details</p>
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:14px;">
      ` + row("Reference", "{{.Ref}}") + `
      ` + row("Date", "{{.Date}}") + `
      ` + row("Electricity company", "{{.Disco}}") + `
      ` + row("Meter", "{{.Meter}}") + `
      {{if .Name}}` + row("Account name", "{{.Name}}") + `{{end}}
      {{if .Token}}` + row("Units", "{{.Kwh}} kWh") + `{{end}}
      ` + row("Electricity", "{{.Electricity}}") + `
      {{if .Fee}}` + row("Service fee", "{{.Fee}}") + `{{end}}
      ` + row("Paid with", "{{.Paid}}") + `
      <tr><td style="padding:16px 0 0;font-weight:bold;color:#1a1a1a;">Total paid</td><td align="right" style="padding:16px 0 0;font-size:19px;font-weight:bold;color:#1a1a1a;">{{.Total}}</td></tr>
    </table>
  </td></tr>
  {{if .ReceiptURL}}<tr><td class="pad" align="center" style="padding:26px 32px 6px;">
    <a href="{{.ReceiptURL}}" style="display:inline-block;background:#111111;color:#ffffff;text-decoration:none;font-size:14px;font-weight:bold;padding:13px 30px;border-radius:6px;">View receipt</a>
  </td></tr>{{end}}` + emailSignoff + emailFoot))

var resetEmailTmpl = template.Must(template.New("reset").Parse(emailHead + `
  <tr><td class="pad" align="center" style="background:#111111;padding:34px 32px 32px;">
    <p style="margin:0;font-size:15px;color:#d6d6d6;">Password reset</p>
    <p class="big" style="margin:12px 0 0;font-size:28px;line-height:1.25;font-weight:bold;color:#ffffff;">Set a new password</p>
  </td></tr>
  <tr><td class="pad" style="padding:30px 32px 0;font-size:15px;line-height:1.6;color:#444444;">
    <p style="margin:0 0 12px;">{{.Hello}}</p>
    <p style="margin:0;">We got a request to reset the password for <b>{{.Email}}</b>. Click the button below to choose a new one.</p>
  </td></tr>
  <tr><td class="pad" align="center" style="padding:28px 32px 0;">
    <a href="{{.Link}}" style="display:inline-block;background:#111111;color:#ffffff;text-decoration:none;font-size:15px;font-weight:bold;padding:14px 34px;border-radius:6px;">Reset password</a>
    <p style="margin:12px 0 0;font-size:12px;color:#8a8a8a;">The link works once and expires in 30 minutes.</p>
  </td></tr>
  <tr><td class="pad" style="padding:26px 32px 0;font-size:13px;line-height:1.6;color:#6b6b6b;">
    <p style="margin:0;">Didn't ask for this? Ignore this email and your password stays the same.</p>
    <p style="margin:12px 0 0;word-break:break-all;">Button not working? Paste this link into your browser:<br><a href="{{.Link}}" style="color:#6b6b6b;">{{.Link}}</a></p>
  </td></tr>` + emailSignoff + emailFoot))

// footerLinks is the site link (and Terms/Privacy) under the email.
func footerLinks(site, terms, privacy string) template.HTML {
	if site == "" {
		return ""
	}
	a := func(href, text string) string {
		return `<a href="` + template.HTMLEscapeString(href) + `" style="color:#8a8a8a;text-decoration:none;">` + template.HTMLEscapeString(text) + `</a>`
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
	intro := "If you have any issue with this purchase, reply to this email"
	if e.Support != "" {
		intro += " or write to " + e.Support
	}
	return execEmail(tokenEmailTmpl, struct {
		tokenEmail
		Intro  string
		Footer template.HTML
	}{e, intro + ".", footerLinks(e.SiteURL, e.TermsURL, e.PrivacyURL)})
}

func renderResetEmail(e resetEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = siteHost(e.SiteURL)
	return execEmail(resetEmailTmpl, struct {
		resetEmail
		Preheader, Intro string
		Footer           template.HTML
	}{e, "Set a new password for " + e.Email + ". The link expires in 30 minutes.",
		"You asked to reset your " + e.BrandName + " password.", footerLinks(e.SiteURL, "", "")})
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
