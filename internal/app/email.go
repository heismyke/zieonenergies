package app

import (
	"bytes"
	"html/template"
	"strings"
)

// tokenEmail is the data for the branded token receipt email.
type tokenEmail struct {
	Brand, Logo, Accent  string
	BrandName            string // as written, e.g. "Zenerlink"
	Preheader, Headline  string
	Token                string   // grouped; empty for postpaid
	Chunks               []string // the token in groups of four
	Kwh                  string   // "79.9"
	Meter, Disco, Name   string
	Date, Ref, Paid      string
	Electricity, Fee     string // Fee is empty when there is none
	Total                string
	ReceiptURL, SiteURL  string
	SiteHost, Support    string
	TermsURL, PrivacyURL string
}

// The email is an energy voucher: a meter-style kWh readout, the token in
// blocks of four on a tear-off ticket, and loading steps. Table layout and
// inline styles only, since Gmail and Outlook drop most CSS; the one <style>
// rule only tightens spacing on phones.
var tokenEmailTmpl = template.Must(template.New("token").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark"><meta name="supported-color-schemes" content="dark"><title>{{.Headline}}</title>
<style>@media (max-width:480px){.pad{padding-left:18px!important;padding-right:18px!important}.chip{font-size:17px!important;padding:10px 0!important}.kwh{font-size:46px!important}}</style></head>
<body style="margin:0;padding:0;background:#050505;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:#050505;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#050505;">
<tr><td align="center" style="padding:28px 10px 36px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:540px;font-family:Helvetica,Arial,sans-serif;color:#ffffff;">

<!-- brand bar -->
<tr><td class="pad" style="padding:0 24px 18px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td valign="middle">{{if .Logo}}<img src="{{.Logo}}" width="30" height="30" alt="" style="display:inline-block;vertical-align:middle;border-radius:8px;border:0;">&nbsp;&nbsp;{{end}}<span style="vertical-align:middle;font-size:13px;font-weight:bold;letter-spacing:4px;color:#ffffff;">{{.Brand}}</span></td>
    <td valign="middle" align="right" style="font-family:'Courier New',Courier,monospace;font-size:12px;color:#8c8c8c;">N&ordm; {{.Ref}}</td>
  </tr></table>
</td></tr>

<!-- voucher -->
<tr><td style="background:#111111;border:1px solid #2a2a2a;border-radius:20px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">

  <!-- meter readout -->
  <tr><td class="pad" align="center" style="padding:30px 28px 26px;background:#17110a;background-image:linear-gradient(180deg,#1f160c 0%,#111111 100%);border-radius:20px 20px 0 0;">
    <p style="margin:0;font-size:11px;font-weight:bold;letter-spacing:3px;color:{{.Accent}};">&#9889; {{if .Token}}ENERGY LOADED{{else}}BILL PAID{{end}}</p>
    {{if .Token}}
    <p class="kwh" style="margin:14px 0 0;font-family:'Courier New',Courier,monospace;font-size:58px;line-height:1;font-weight:bold;color:#ffffff;letter-spacing:-1px;">{{.Kwh}}<span style="font-family:Helvetica,Arial,sans-serif;font-size:18px;letter-spacing:1px;color:{{.Accent}};">&nbsp;kWh</span></p>
    {{else}}
    <p class="kwh" style="margin:14px 0 0;font-size:46px;line-height:1;font-weight:bold;color:#ffffff;">{{.Electricity}}</p>
    {{end}}
    <p style="margin:12px 0 0;font-size:14px;color:#a8a8a8;">{{.Disco}} &nbsp;&middot;&nbsp; meter <span style="color:#ffffff;">{{.Meter}}</span></p>
    {{if .Name}}<p style="margin:4px 0 0;font-size:12px;letter-spacing:1px;color:#6c6c6c;">{{.Name}}</p>{{end}}
  </td></tr>

  {{if .Token}}
  <!-- token -->
  <tr><td class="pad" style="padding:26px 28px 6px;">
    <p style="margin:0 0 12px;font-size:11px;letter-spacing:3px;color:#8c8c8c;" align="center">YOUR TOKEN</p>
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      {{range $i, $c := .Chunks}}{{if $i}}<td width="6" style="font-size:0;">&nbsp;</td>{{end}}<td class="chip" align="center" style="background:#050505;border:1px solid #3a2a1b;border-radius:10px;padding:13px 0;font-family:'Courier New',Courier,monospace;font-size:21px;font-weight:bold;color:#ffffff;">{{$c}}</td>{{end}}
    </tr></table>
    <p style="margin:10px 0 0;font-family:'Courier New',Courier,monospace;font-size:11px;color:#5c5c5c;" align="center">{{.Token}}</p>
  </td></tr>

  <!-- how to load -->
  <tr><td class="pad" style="padding:18px 28px 4px;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:13px;color:#a8a8a8;">
      <tr><td width="28" valign="top" style="padding:5px 0;"><span style="display:inline-block;width:20px;height:20px;line-height:20px;border-radius:10px;background:{{.Accent}};color:#050505;font-size:11px;font-weight:bold;text-align:center;">1</span></td><td style="padding:5px 0;">Type the 20 digits on your meter keypad.</td></tr>
      <tr><td width="28" valign="top" style="padding:5px 0;"><span style="display:inline-block;width:20px;height:20px;line-height:20px;border-radius:10px;background:{{.Accent}};color:#050505;font-size:11px;font-weight:bold;text-align:center;">2</span></td><td style="padding:5px 0;">Press <b style="color:#ffffff;">Enter</b> (or <b style="color:#ffffff;">#</b> on some meters).</td></tr>
      <tr><td width="28" valign="top" style="padding:5px 0;"><span style="display:inline-block;width:20px;height:20px;line-height:20px;border-radius:10px;background:{{.Accent}};color:#050505;font-size:11px;font-weight:bold;text-align:center;">3</span></td><td style="padding:5px 0;">Wait for <b style="color:#ffffff;">Accept</b> and your new balance.</td></tr>
    </table>
  </td></tr>
  {{end}}

  <!-- perforation: notches on both sides with a dashed tear line -->
  <tr><td style="padding:22px 0 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      <td width="14" height="28" style="background:#050505;border-radius:0 14px 14px 0;font-size:0;">&nbsp;</td>
      <td style="padding:0 10px;"><div style="border-top:2px dashed #2e2e2e;height:0;line-height:0;font-size:0;">&nbsp;</div></td>
      <td width="14" height="28" style="background:#050505;border-radius:14px 0 0 14px;font-size:0;">&nbsp;</td>
    </tr></table>
  </td></tr>

  <!-- stub -->
  <tr><td class="pad" style="padding:18px 28px 6px;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:13px;">
      <tr><td style="padding:5px 0;color:#8c8c8c;">Electricity</td><td align="right" style="padding:5px 0;color:#ffffff;">{{.Electricity}}</td></tr>
      {{if .Fee}}<tr><td style="padding:5px 0;color:#8c8c8c;">Service fee</td><td align="right" style="padding:5px 0;color:#ffffff;">{{.Fee}}</td></tr>{{end}}
      <tr><td style="padding:5px 0;color:#8c8c8c;">Paid with</td><td align="right" style="padding:5px 0;color:#ffffff;">{{.Paid}}</td></tr>
      <tr><td style="padding:5px 0;color:#8c8c8c;">Date</td><td align="right" style="padding:5px 0;color:#ffffff;">{{.Date}}</td></tr>
      <tr><td style="padding:14px 0 0;border-top:1px solid #222222;font-size:12px;letter-spacing:2px;color:#8c8c8c;">TOTAL PAID</td><td align="right" style="padding:14px 0 0;border-top:1px solid #222222;font-size:22px;font-weight:bold;color:{{.Accent}};">{{.Total}}</td></tr>
    </table>
  </td></tr>

  {{if .ReceiptURL}}
  <tr><td class="pad" style="padding:24px 28px 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      <td align="center" style="background:{{.Accent}};border-radius:12px;"><a href="{{.ReceiptURL}}" style="display:block;padding:14px 0;color:#050505;text-decoration:none;font-size:14px;font-weight:bold;">Open receipt &amp; share token &rarr;</a></td>
    </tr></table>
  </td></tr>
  {{end}}
  <tr><td class="pad" align="center" style="padding:18px 28px 26px;font-size:12px;line-height:1.5;color:#6c6c6c;">
    Meter not accepting it?{{if .Support}} Email <a href="mailto:{{.Support}}" style="color:#a8a8a8;">{{.Support}}</a> and quote N&ordm; {{.Ref}}.{{else}} Reply with N&ordm; {{.Ref}}.{{end}}
  </td></tr>
  </table>
</td></tr>

<!-- footer: a power line -->
<tr><td style="padding:26px 24px 0;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td height="1" style="background:#2a2a2a;background-image:linear-gradient(90deg,#050505 0%,{{.Accent}} 50%,#050505 100%);font-size:0;line-height:0;">&nbsp;</td>
  </tr></table>
</td></tr>
<tr><td align="center" style="padding:16px 24px 0;font-size:11px;line-height:1.7;color:#5c5c5c;">
  Sent because you bought electricity on {{.BrandName}}.<br>
  {{if .SiteURL}}<a href="{{.SiteURL}}" style="color:#8c8c8c;text-decoration:none;">{{.SiteHost}}</a>{{end}}{{if .TermsURL}} &nbsp;&middot;&nbsp; <a href="{{.TermsURL}}" style="color:#8c8c8c;text-decoration:none;">Terms</a> &nbsp;&middot;&nbsp; <a href="{{.PrivacyURL}}" style="color:#8c8c8c;text-decoration:none;">Privacy</a>{{end}}
</td></tr>

</table>
</td></tr></table>
</body></html>`))

func renderTokenEmail(e tokenEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = strings.TrimPrefix(strings.TrimPrefix(e.SiteURL, "https://"), "http://")
	if e.Token != "" {
		e.Chunks = strings.Split(e.Token, "-")
	}
	var b bytes.Buffer
	if err := tokenEmailTmpl.Execute(&b, e); err != nil {
		return ""
	}
	return b.String()
}

// resetEmail is the data for the password reset email.
type resetEmail struct {
	Brand, BrandName, Logo, Accent string
	Hello, Email, Link             string
	Support, SiteURL, SiteHost     string
}

var resetEmailTmpl = template.Must(template.New("reset").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark"><meta name="supported-color-schemes" content="dark"><title>Reset your {{.BrandName}} password</title>
<style>@media (max-width:480px){.pad{padding-left:20px!important;padding-right:20px!important}}</style></head>
<body style="margin:0;padding:0;background:#050505;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:#050505;">Set a new password for {{.Email}}. The link expires in 30 minutes.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#050505;">
<tr><td align="center" style="padding:28px 10px 36px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:540px;font-family:Helvetica,Arial,sans-serif;color:#ffffff;">

<tr><td class="pad" style="padding:0 24px 18px;">
  {{if .Logo}}<img src="{{.Logo}}" width="30" height="30" alt="" style="display:inline-block;vertical-align:middle;border-radius:8px;border:0;">&nbsp;&nbsp;{{end}}<span style="vertical-align:middle;font-size:13px;font-weight:bold;letter-spacing:4px;color:#ffffff;">{{.Brand}}</span>
</td></tr>

<tr><td style="background:#111111;border:1px solid #2a2a2a;border-radius:20px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
  <tr><td class="pad" style="padding:30px 28px 8px;background:#17110a;background-image:linear-gradient(180deg,#1f160c 0%,#111111 100%);border-radius:20px 20px 0 0;">
    <p style="margin:0;font-size:11px;font-weight:bold;letter-spacing:3px;color:{{.Accent}};">&#128274; PASSWORD RESET</p>
    <h1 style="margin:14px 0 0;font-size:26px;line-height:1.25;font-weight:bold;color:#ffffff;">Set a new password</h1>
  </td></tr>
  <tr><td class="pad" style="padding:16px 28px 0;font-size:15px;line-height:1.6;color:#a8a8a8;">
    <p style="margin:0 0 12px;">{{.Hello}}</p>
    <p style="margin:0;">We got a request to reset the password for <span style="color:#ffffff;">{{.Email}}</span>. Tap the button to choose a new one.</p>
  </td></tr>
  <tr><td class="pad" style="padding:26px 28px 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      <td align="center" style="background:{{.Accent}};border-radius:12px;"><a href="{{.Link}}" style="display:block;padding:15px 0;color:#050505;text-decoration:none;font-size:15px;font-weight:bold;">Reset password &rarr;</a></td>
    </tr></table>
    <p style="margin:12px 0 0;font-size:12px;color:#6c6c6c;" align="center">Works once &middot; expires in 30 minutes</p>
  </td></tr>
  <tr><td style="padding:22px 0 0;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      <td width="14" height="28" style="background:#050505;border-radius:0 14px 14px 0;font-size:0;">&nbsp;</td>
      <td style="padding:0 10px;"><div style="border-top:2px dashed #2e2e2e;height:0;line-height:0;font-size:0;">&nbsp;</div></td>
      <td width="14" height="28" style="background:#050505;border-radius:14px 0 0 14px;font-size:0;">&nbsp;</td>
    </tr></table>
  </td></tr>
  <tr><td class="pad" style="padding:16px 28px 26px;font-size:12px;line-height:1.6;color:#6c6c6c;">
    Didn't ask for this? Ignore this email and your password stays the same.{{if .Support}} If you're worried, email <a href="mailto:{{.Support}}" style="color:#a8a8a8;">{{.Support}}</a>.{{end}}
    <p style="margin:12px 0 0;word-break:break-all;">Button not working? Paste this link into your browser:<br><a href="{{.Link}}" style="color:#8c8c8c;">{{.Link}}</a></p>
  </td></tr>
  </table>
</td></tr>

<tr><td style="padding:26px 24px 0;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td height="1" style="background:#2a2a2a;background-image:linear-gradient(90deg,#050505 0%,{{.Accent}} 50%,#050505 100%);font-size:0;line-height:0;">&nbsp;</td>
  </tr></table>
</td></tr>
<tr><td align="center" style="padding:16px 24px 0;font-size:11px;line-height:1.7;color:#5c5c5c;">
  Sent because someone asked to reset a {{.BrandName}} password.<br>
  {{if .SiteURL}}<a href="{{.SiteURL}}" style="color:#8c8c8c;text-decoration:none;">{{.SiteHost}}</a>{{end}}
</td></tr>

</table>
</td></tr></table>
</body></html>`))

func renderResetEmail(e resetEmail) string {
	if !hexColor.MatchString(e.Accent) {
		e.Accent = "#C8823A"
	}
	e.SiteHost = strings.TrimPrefix(strings.TrimPrefix(e.SiteURL, "https://"), "http://")
	var b bytes.Buffer
	if err := resetEmailTmpl.Execute(&b, e); err != nil {
		return ""
	}
	return b.String()
}
