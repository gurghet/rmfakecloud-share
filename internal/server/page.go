package server

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/gurghet/rmfakecloud-share/internal/store"
)

// md renders Markdown without raw HTML passthrough (no html.WithUnsafe), so
// model output can never inject markup or script. Dangerous link schemes such
// as javascript: are also dropped by goldmark in this mode.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

func renderMarkdown(src string) (template.HTML, error) {
	var b bytes.Buffer
	if err := md.Convert([]byte(src), &b); err != nil {
		return "", err
	}
	// Every anchor goldmark emits starts with "<a href=" (a literal "<a " in the
	// source is escaped), so the attribute can be added without parsing the HTML.
	out := strings.ReplaceAll(b.String(), "<a href=", `<a rel="nofollow noopener noreferrer" href=`)
	return template.HTML(out), nil //nolint:gosec // goldmark safe mode output
}

type pageData struct {
	Nonce      string
	ID         string
	Pending    bool
	Failed     bool
	Disabled   bool
	Done       bool
	Note       template.HTML
	PageNums   []int
	Created    string
	Model      string
	AutoReload bool
}

func (s *srv) handlePage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	d := pageData{Nonce: newNonce(), ID: m.ID, Created: m.Created.UTC().Format("2 Jan 2006, 15:04 UTC"), Model: m.Model}
	for i := 1; i <= m.PageCount; i++ {
		d.PageNums = append(d.PageNums, i)
	}
	switch m.Status {
	case store.StatusDone:
		note, err := s.Store.Note(m.ID)
		if err != nil {
			s.Log.Error("reading note failed", "share", m.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		html, err := renderMarkdown(note)
		if err != nil {
			s.Log.Error("rendering note failed", "share", m.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		d.Done, d.Note = true, html
	case store.StatusFailed:
		d.Failed = true // the cause stays in the log and meta.json, never on the public page
	default:
		if s.Enqueuer == nil {
			d.Disabled = true
		} else {
			d.Pending, d.AutoReload = true, true
		}
	}
	var b bytes.Buffer
	if err := pageTmpl.Execute(&b, d); err != nil {
		s.Log.Error("rendering page failed", "share", m.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	setCommonHeaders(h)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// No external assets, no inline script or style without this response's nonce.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; connect-src 'self'; "+
		"style-src 'nonce-"+d.Nonce+"'; script-src 'nonce-"+d.Nonce+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write(b.Bytes())
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="no-referrer">
{{if .AutoReload}}<meta http-equiv="refresh" content="5">{{end}}
<title>Shared note</title>
<style nonce="{{.Nonce}}">
:root{color-scheme:light dark;--bg:#fbfbfa;--fg:#1c1c1e;--muted:#6b6b70;--card:#fff;--line:#e2e2e0;--accent:#0b5fff;--warn:#8a4b00;--warnbg:#fff4e0;--err:#9b1c1c;--errbg:#fdecec}
@media (prefers-color-scheme:dark){:root{--bg:#121214;--fg:#e8e8ea;--muted:#9a9aa2;--card:#1b1b1f;--line:#2f2f35;--accent:#7aa7ff;--warn:#f0c27a;--warnbg:#2b2214;--err:#ff9b9b;--errbg:#2e1818}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:17px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:46rem;margin:0 auto;padding:16px 16px 48px}
header{margin:8px 0 16px}
header h1{font-size:1rem;font-weight:600;margin:0;color:var(--muted)}
header p{margin:2px 0 0;font-size:.85rem;color:var(--muted)}
.status{border-radius:10px;padding:12px 14px;margin:0 0 16px;font-size:.95rem}
.status.pending,.status.disabled{background:var(--warnbg);color:var(--warn)}
.status.failed{background:var(--errbg);color:var(--err)}
.note{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:4px 18px 8px;overflow-wrap:anywhere}
.note h1,.note h2,.note h3{line-height:1.25}
.note img{max-width:100%}
.note pre{overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:10px;font-size:.85rem}
.note code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.9em}
.note table{border-collapse:collapse;display:block;overflow-x:auto}
.note th,.note td{border:1px solid var(--line);padding:4px 8px}
.note blockquote{margin-left:0;padding-left:12px;border-left:3px solid var(--line);color:var(--muted)}
.actions{display:flex;flex-wrap:wrap;gap:8px;margin:0 0 12px}
.btn{font:inherit;font-size:.9rem;padding:8px 14px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--accent);text-decoration:none;cursor:pointer}
.btn[hidden]{display:none}
details{margin-top:20px;border:1px solid var(--line);border-radius:12px;background:var(--card)}
summary{cursor:pointer;padding:12px 14px;font-weight:600;font-size:.95rem}
.pages{padding:0 12px 12px}
.pages img{display:block;width:100%;height:auto;margin:8px 0;border:1px solid var(--line);border-radius:6px;background:#fff}
footer{margin-top:24px;font-size:.8rem;color:var(--muted)}
</style>
</head>
<body>
<main>
<header>
<h1>Note shared from a reMarkable tablet</h1>
<p>{{.Created}} · {{len .PageNums}} page{{if ne (len .PageNums) 1}}s{{end}}</p>
</header>
{{if .Pending}}<div class="status pending" role="status">Converting your handwriting to text… this page refreshes by itself.</div>{{end}}
{{if .Disabled}}<div class="status disabled" role="status">Conversion disabled on this server. The original pages are shown below.</div>{{end}}
{{if .Failed}}<div class="status failed" role="alert">Conversion failed. The original pages are shown below.</div>{{end}}
{{if .Done}}
<div class="actions">
<button class="btn" id="copy" type="button" hidden>Copy Markdown</button>
<a class="btn" href="note.md" download="note.md">Download Markdown</a>
</div>
<article class="note">{{.Note}}</article>
{{end}}
<details{{if not .Done}} open{{end}}>
<summary>Original pages ({{len .PageNums}})</summary>
<div class="pages">
{{range .PageNums}}<img src="pages/{{.}}.png" alt="Page {{.}}" loading="lazy">
{{end}}</div>
</details>
<footer>{{if .Done}}Transcribed by {{if .Model}}{{.Model}}{{else}}a language model{{end}}. Handwriting recognition can be wrong, so check against the original pages.<br>{{end}}Served by rmfakecloud-share, an independent open-source project, not affiliated with reMarkable AS.</footer>
</main>
{{if .Done}}<script nonce="{{.Nonce}}">
(function(){var b=document.getElementById('copy');if(!b||!navigator.clipboard)return;b.hidden=false;
b.addEventListener('click',function(){fetch('note.md').then(function(r){return r.text()}).then(function(t){return navigator.clipboard.writeText(t)}).then(function(){b.textContent='Copied'}).catch(function(){b.textContent='Copy failed'})})})();
</script>{{end}}
</body>
</html>
`))
