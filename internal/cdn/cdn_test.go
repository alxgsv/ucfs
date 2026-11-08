package cdn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOutputExt(t *testing.T) {
	for ops, want := range map[string]string{
		"":                              "",
		"-/":                            "",
		"-/resize/1200x/":               "",
		"-/format/":                     "",
		"-/resize/1200x/-/format/webp/": "webp",
		"-/format/jpeg/":                "jpg",
		"-/format/png/-/format/auto/":   "",
		"-/format/webp/-/resize/10x/":   "webp",
		"-/resize/100x/-/format/webp/-/format/png/": "png",
		"-/preview/-/main_colors/3/":                "json",
		"-/resize/100x/-/json/":                     "json",
		"-/text/10x10/0,0/format/-/":                "",
	} {
		if got := OutputExt(ops); got != want {
			t.Errorf("OutputExt(%q) = %q, want %q", ops, got, want)
		}
	}
}

func TestURLEscapesArguments(t *testing.T) {
	c := New("https://cdn.example/", nil, nil)
	for ops, want := range map[string]string{
		"":                       "https://cdn.example/u/",
		"-/resize/1200x/":        "https://cdn.example/u/-/resize/1200x/",
		"-/text/10x10/0,0/a b?/": "https://cdn.example/u/-/text/10x10/0,0/a%20b%3F/",
		"-/crop/1:1/50%/":        "https://cdn.example/u/-/crop/1:1/50%25/",
	} {
		if got := c.URL("u", ops); got != want {
			t.Errorf("URL(%q) = %q, want %q", ops, got, want)
		}
	}
}

// page is the shape of the CDN's 400 page, trimmed of its style sheet.
const page = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>400: Bad Request</title>
<style>
h3 { color: black }
</style>
</head>
<body>
<h3>Bad Request</h3>


<p>Mandatory argument &quot;dimensions&quot; not found while parsing &quot;resize/abc&quot;</p>



<p>
See <a href="https://uploadcare.com/docs/delivery/cdn/">CDN documentation</a>.
</p>

</body>
</html>
`

func TestBadRequestOnHeadFetchesTheMessage(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	c := New(srv.URL, nil, nil)
	_, err := c.Head(context.Background(), "u", "-/resize/abc/")
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a BadRequestError", err)
	}
	if want := `Mandatory argument "dimensions" not found while parsing "resize/abc"`; bad.Message != want {
		t.Errorf("message = %q, want %q", bad.Message, want)
	}
	if st := c.Stats(); st.Heads != 1 || st.Gets != 1 || st.Bytes != int64(len(page)) || len(methods) != 2 {
		t.Errorf("stats = %+v, methods %v; want one HEAD and one GET for the message", st, methods)
	}
	if _, _, err := c.ReadRange(context.Background(), "u", "-/resize/abc/", make([]byte, 10), 0); !errors.As(err, &bad) {
		t.Errorf("ranged GET: err = %v, want a BadRequestError", err)
	}
}
