package templates

// emailCSS is the single <style> block injected into the layout <head> and
// inlined by go-premailer at render time. Keep selectors simple (class/element)
// so premailer can inline them; the @media rule is intentionally left in the
// <style> block (premailer cannot inline media queries).
const emailCSS = `
body { margin:0; padding:0; background-color:#f7f8fa; }
.canvas { background-color:#f7f8fa; width:100%; }
.container { max-width:480px; margin:0 auto; padding:24px 16px; }
.header { padding:8px 4px 16px; }
.logo { width:36px; height:36px; border-radius:12px; vertical-align:middle; }
.wordmark { font-family:` + FontSans + `; font-size:18px; font-weight:500; letter-spacing:-0.025em; color:#151b24; padding-left:12px; vertical-align:middle; }
.card { background-color:#fdfdfe; border:1px solid #d4d8de; border-radius:10px; padding:28px 24px; }
.h1 { font-family:` + FontSans + `; font-size:22px; font-weight:500; letter-spacing:-0.013em; color:#151b24; margin:0 0 12px; }
.p { font-family:` + FontSans + `; font-size:16px; line-height:1.55; color:#151b24; margin:0 0 16px; }
.muted { font-family:` + FontSans + `; font-size:13px; line-height:1.5; color:#6b727e; }
.btn-cell { border-radius:12px; }
.btn { display:block; font-family:` + FontSans + `; font-size:14px; line-height:20px; font-weight:500; color:#f7f8fc; background-color:#3c68d9; text-decoration:none; text-align:center; padding:8px 22px; border-radius:12px; }
.chip { font-family:` + FontMono + `; font-size:13px; color:#151b24; background-color:#e7ebf2; border-radius:8px; padding:10px 12px; word-break:break-all; }
.divider { border:0; border-top:1px solid #d4d8de; margin:24px 0; }
.footer { padding:16px 4px 0; }
@media (max-width:600px) {
  .container { padding:16px 12px; }
  .card { padding:20px 16px; }
}
`

// styleTag wraps emailCSS in a <style> element for injection into the layout
// <head> via @templ.Raw. templ treats a literal <style> as a raw-text element
// and will NOT interpret a templ expression placed inside it, so the whole tag
// is emitted as raw HTML instead. go-premailer reads this <style> from the
// rendered document and inlines it.
const styleTag = `<style type="text/css">` + emailCSS + `</style>`
