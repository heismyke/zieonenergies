package app

import (
	"bytes"
	"html/template"
	"strings"
)

// tokenEmail is the data for the branded token receipt email.
type tokenEmail struct {
	Brand, Logo, Accent      string
	Preheader, Headline, Sub string
	Token                    string // grouped; empty for postpaid
	Rows                     [][2]string
	Total                    string
	ReceiptURL, SiteURL      string
	SiteHost, Support        string
	TermsURL, PrivacyURL     string
}

// Table layout and inline styles only: Gmail and Outlook drop <style> rules
// and most modern CSS. Colours follow the tenant theme.
var tokenEmailTmpl = template.Must(template.New("token").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark"><meta name="supported-color-schemes" content="dark"><title>{{.Headline}}</title>
<style>@media (max-width:480px){.tok{font-size:19px!important;letter-spacing:0!important}.pad{padding-left:20px!important;padding-right:20px!important}}</style></head>
<body style="margin:0;padding:0;background:#050505;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:#050505;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#050505;">
<tr><td align="center" style="padding:32px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;font-family:Helvetica,Arial,sans-serif;color:#ffffff;">

<tr><td style="padding:0 8px 20px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td valign="middle">{{if .Logo}}<img src="{{.Logo}}" width="32" height="32" alt="" style="display:inline-block;vertical-align:middle;border-radius:8px;border:0;">&nbsp;&nbsp;{{end}}<span style="vertical-align:middle;font-size:14px;font-weight:bold;letter-spacing:4px;color:#ffffff;">{{.Brand}}</span></td>
    <td valign="middle" align="right" style="font-size:11px;letter-spacing:2px;color:#8c8c8c;">RECEIPT</td>
  </tr></table>
</td></tr>

<tr><td style="background:#111111;border:1px solid #222222;border-radius:16px;overflow:hidden;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
  <tr><td height="3" style="background:{{.Accent}};font-size:0;line-height:0;">&nbsp;</td></tr>
  <tr><td class="pad" style="padding:28px 28px 8px;">
    <p style="margin:0 0 10px;font-size:11px;font-weight:bold;letter-spacing:2px;color:{{.Accent}};">{{if .Token}}TOKEN READY{{else}}PAYMENT RECEIVED{{end}}</p>
    <h1 style="margin:0;font-size:24px;line-height:1.25;font-weight:bold;color:#ffffff;">{{.Headline}}</h1>
    <p style="margin:8px 0 0;font-size:14px;line-height:1.5;color:#8c8c8c;">{{.Sub}}</p>
  </td></tr>
  {{if .Token}}
  <tr><td class="pad" style="padding:20px 28px 4px;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#17110a;border:1px solid {{.Accent}};border-radius:12px;">
    <tr><td align="center" style="padding:20px 12px 6px;font-size:11px;letter-spacing:2px;color:#8c8c8c;">YOUR 20-DIGIT TOKEN</td></tr>
    <tr><td class="tok" align="center" style="padding:0 8px 20px;font-family:'Courier New',Courier,monospace;font-size:24px;font-weight:bold;letter-spacing:1px;color:#ffffff;">{{.Token}}</td></tr>
    </table>
    <p style="margin:12px 0 0;font-size:13px;line-height:1.5;color:#8c8c8c;">Type these 20 digits on your meter keypad, then press <b style="color:#ffffff;">Enter</b>.</p>
  </td></tr>
  {{end}}
  <tr><td class="pad" style="padding:20px 28px 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:14px;">
    {{range .Rows}}<tr><td style="padding:9px 0;border-bottom:1px solid #222222;color:#8c8c8c;">{{index . 0}}</td><td align="right" style="padding:9px 0;border-bottom:1px solid #222222;color:#ffffff;">{{index . 1}}</td></tr>
    {{end}}<tr><td style="padding:14px 0 0;font-weight:bold;color:#ffffff;">Total paid</td><td align="right" style="padding:14px 0 0;font-size:18px;font-weight:bold;color:{{.Accent}};">{{.Total}}</td></tr>
    </table>
  </td></tr>
  {{if .ReceiptURL}}
  <tr><td class="pad" align="center" style="padding:28px 28px 4px;">
    <a href="{{.ReceiptURL}}" style="display:inline-block;background:{{.Accent}};color:#050505;text-decoration:none;font-size:14px;font-weight:bold;padding:13px 28px;border-radius:999px;">View receipt</a>
  </td></tr>
  {{end}}
  <tr><td class="pad" style="padding:20px 28px 28px;font-size:13px;line-height:1.5;color:#8c8c8c;" align="center">
    Token not working?{{if .Support}} Email <a href="mailto:{{.Support}}" style="color:#ffffff;">{{.Support}}</a> with your reference.{{else}} Reply to this email with your reference.{{end}}
  </td></tr>
  </table>
</td></tr>

<tr><td align="center" style="padding:24px 8px 0;font-size:12px;line-height:1.6;color:#5c5c5c;">
  You asked for this token by email when you paid.<br>
  {{if .SiteURL}}<a href="{{.SiteURL}}" style="color:#8c8c8c;text-decoration:none;">{{.SiteHost}}</a>{{end}}{{if .TermsURL}} &nbsp;·&nbsp; <a href="{{.TermsURL}}" style="color:#8c8c8c;text-decoration:none;">Terms</a> &nbsp;·&nbsp; <a href="{{.PrivacyURL}}" style="color:#8c8c8c;text-decoration:none;">Privacy</a>{{end}}
</td></tr>

</table>
</td></tr></table>
</body></html>`))

func renderTokenEmail(e tokenEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = strings.TrimPrefix(strings.TrimPrefix(e.SiteURL, "https://"), "http://")
	var b bytes.Buffer
	if err := tokenEmailTmpl.Execute(&b, e); err != nil {
		return ""
	}
	return b.String()
}
