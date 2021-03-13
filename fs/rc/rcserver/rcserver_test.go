package rcserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/rc"
)

const (
	testBindAddress = "localhost:0"
	testTemplate    = "testdata/golden/testindex.html"
	testFs          = "testdata/files"
	remoteURL       = "[" + testFs + "]/" // initial URL path to fetch from that remote
)

func TestMain(m *testing.M) {
	// Pretend to be rclone version if we have a version string parameter
	if os.Args[len(os.Args)-1] == "version" {
		fmt.Printf("rclone %s\n", fs.Version)
		os.Exit(0)
	}
	// Pretend to error if we have an unknown command
	if os.Args[len(os.Args)-1] == "unknown_command" {
		fmt.Printf("rclone %s\n", fs.Version)
		fmt.Fprintf(os.Stderr, "Unknown command\n")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Test the RC server runs and we can do HTTP fetches from it.
// We'll do the majority of the testing with the httptest framework
func TestRcServer(t *testing.T) {
	opt := rc.DefaultOpt
	opt.HTTPOptions.ListenAddr = testBindAddress
	opt.HTTPOptions.Template = testTemplate
	opt.Enabled = true
	opt.Serve = true
	opt.Files = testFs
	mux := http.NewServeMux()
	rcServer := newServer(context.Background(), &opt, mux)
	assert.NoError(t, rcServer.Serve())
	defer func() {
		rcServer.Close()
		rcServer.Wait()
	}()
	testURL := rcServer.Server.URL()

	// Do the simplest possible test to check the server is alive
	// Do it a few times to wait for the server to start
	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		resp, err = http.Get(testURL + "file.txt")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	require.NoError(t, err)
	body, err := ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "this is file1.txt\n", string(body))
}

type testRun struct {
	Name        string
	URL         string
	Status      int
	Method      string
	Range       string
	Body        string
	ContentType string
	Expected    string
	Contains    *regexp.Regexp
	Headers     map[string]string
	NoAuth      bool
}

// Run a suite of tests
func testServer(t *testing.T, tests []testRun, opt *rc.Options) {
	testServerWithListen(t, tests, opt, false)
}

const (
	minListenPort = 32000
	maxListenPort = 48000
	portAttempts  = 100
)

func testServerWithListen(t *testing.T, tests []testRun, opt *rc.Options, listen bool) {
	ctx := context.Background()
	configfile.LoadConfig(ctx)
	mux := http.NewServeMux()
	opt.HTTPOptions.Template = testTemplate
	rcServer := newServer(ctx, opt, mux)
	baseURL := "http://1.2.3.4/"

	if listen {
		// Find a free local listen port
		var err error
		for attempt := 0; attempt < portAttempts; attempt++ {
			port := minListenPort + rand.Intn(maxListenPort-minListenPort)
			addr := fmt.Sprintf("localhost:%d", port)
			baseURL = fmt.Sprintf("http://%s/", addr)
			var ln net.Listener
			if ln, err = net.Listen("tcp", addr); err == nil {
				opt.HTTPOptions.ListenAddr = addr
				require.NoError(t, ln.Close())
				time.Sleep(10 * time.Millisecond) // kernel closes listeners lazily
				break
			}
		}
		require.NoError(t, err, "cannot find free listen port")

		mux = http.DefaultServeMux
		rcServer = newServer(ctx, opt, mux)
		require.NoError(t, rcServer.Serve(), "cannot start local server at %s", baseURL)
		defer func() {
			rcServer.Close()
		}()
	}

	client := fshttp.NewClient(ctx)
	basicUser := opt.HTTPOptions.BasicUser
	basicPass := opt.HTTPOptions.BasicPass

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			method := test.Method
			if method == "" {
				method = "GET"
			}
			var inBody io.Reader
			if test.Body != "" {
				buf := bytes.NewBufferString(test.Body)
				inBody = buf
			}
			req, err := http.NewRequest(method, baseURL+test.URL, inBody)
			require.NoError(t, err)
			if test.Range != "" {
				req.Header.Add("Range", test.Range)
			}
			if test.ContentType != "" {
				req.Header.Add("Content-Type", test.ContentType)
			}

			if !test.NoAuth && basicUser != "" && basicPass != "" {
				req.SetBasicAuth(basicUser, basicPass)
			}
			var resp *http.Response
			if listen {
				// Test full network stack on loopback port
				resp, err = client.Do(req)
				require.NoError(t, err)
			} else {
				// Bypass network stack, call handler directly
				w := httptest.NewRecorder()
				rcServer.handler(w, req)
				resp = w.Result()
			}

			assert.Equal(t, test.Status, resp.StatusCode)
			body, err := ioutil.ReadAll(resp.Body)
			require.NoError(t, err)

			if test.Contains == nil {
				assert.Equal(t, test.Expected, string(body))
			} else {
				assert.True(t, test.Contains.Match(body), fmt.Sprintf("body didn't match: %v: %v", test.Contains, string(body)))
			}

			for k, v := range test.Headers {
				assert.Equal(t, v, resp.Header.Get(k), k)
			}
		})
	}
}

// return an enabled rc
func newTestOpt() rc.Options {
	opt := rc.DefaultOpt
	opt.Enabled = true
	return opt
}

func TestFileServing(t *testing.T) {
	tests := []testRun{{
		Name:   "index",
		URL:    "",
		Status: http.StatusOK,
		Expected: `<pre>
<a href="dir/">dir/</a>
<a href="file.txt">file.txt</a>
</pre>
`,
	}, {
		Name:     "notfound",
		URL:      "notfound",
		Status:   http.StatusNotFound,
		Expected: "404 page not found\n",
	}, {
		Name:     "dirnotfound",
		URL:      "dirnotfound/",
		Status:   http.StatusNotFound,
		Expected: "404 page not found\n",
	}, {
		Name:   "dir",
		URL:    "dir/",
		Status: http.StatusOK,
		Expected: `<pre>
<a href="file2.txt">file2.txt</a>
</pre>
`,
	}, {
		Name:     "file",
		URL:      "file.txt",
		Status:   http.StatusOK,
		Expected: "this is file1.txt\n",
		Headers: map[string]string{
			"Content-Length": "18",
		},
	}, {
		Name:     "file2",
		URL:      "dir/file2.txt",
		Status:   http.StatusOK,
		Expected: "this is dir/file2.txt\n",
	}, {
		Name:     "file-head",
		URL:      "file.txt",
		Method:   "HEAD",
		Status:   http.StatusOK,
		Expected: ``,
		Headers: map[string]string{
			"Content-Length": "18",
		},
	}, {
		Name:     "file-range",
		URL:      "file.txt",
		Status:   http.StatusPartialContent,
		Range:    "bytes=8-12",
		Expected: `file1`,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestRemoteServing(t *testing.T) {
	tests := []testRun{
		// Test serving files from the test remote
		{
			Name:   "index",
			URL:    remoteURL + "",
			Status: http.StatusOK,
			Expected: `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Directory listing of /</title>
</head>
<body>
<h1>Directory listing of /</h1>
<a href="dir/">dir/</a><br />
<a href="file.txt">file.txt</a><br />
</body>
</html>
`,
		}, {
			Name:   "notfound-index",
			URL:    "[notfound]/",
			Status: http.StatusNotFound,
			Expected: `{
	"error": "failed to list directory: directory not found",
	"input": null,
	"path": "",
	"status": 404
}
`,
		}, {
			Name:   "notfound",
			URL:    remoteURL + "notfound",
			Status: http.StatusNotFound,
			Expected: `{
	"error": "failed to find object: object not found",
	"input": null,
	"path": "notfound",
	"status": 404
}
`,
		}, {
			Name:   "dirnotfound",
			URL:    remoteURL + "dirnotfound/",
			Status: http.StatusNotFound,
			Expected: `{
	"error": "failed to list directory: directory not found",
	"input": null,
	"path": "dirnotfound",
	"status": 404
}
`,
		}, {
			Name:   "dir",
			URL:    remoteURL + "dir/",
			Status: http.StatusOK,
			Expected: `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Directory listing of /dir</title>
</head>
<body>
<h1>Directory listing of /dir</h1>
<a href="file2.txt">file2.txt</a><br />
</body>
</html>
`,
		}, {
			Name:     "file",
			URL:      remoteURL + "file.txt",
			Status:   http.StatusOK,
			Expected: "this is file1.txt\n",
			Headers: map[string]string{
				"Content-Length": "18",
			},
		}, {
			Name:     "file with no slash after ]",
			URL:      strings.TrimRight(remoteURL, "/") + "file.txt",
			Status:   http.StatusOK,
			Expected: "this is file1.txt\n",
			Headers: map[string]string{
				"Content-Length": "18",
			},
		}, {
			Name:     "file2",
			URL:      remoteURL + "dir/file2.txt",
			Status:   http.StatusOK,
			Expected: "this is dir/file2.txt\n",
		}, {
			Name:     "file-head",
			URL:      remoteURL + "file.txt",
			Method:   "HEAD",
			Status:   http.StatusOK,
			Expected: ``,
			Headers: map[string]string{
				"Content-Length": "18",
			},
		}, {
			Name:     "file-range",
			URL:      remoteURL + "file.txt",
			Status:   http.StatusPartialContent,
			Range:    "bytes=8-12",
			Expected: `file1`,
		}, {
			Name:   "bad-remote",
			URL:    "[notfoundremote:]/",
			Status: http.StatusInternalServerError,
			Expected: `{
	"error": "failed to make Fs: didn't find section in config file",
	"input": null,
	"path": "/",
	"status": 500
}
`,
		}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestRC(t *testing.T) {
	tests := []testRun{{
		Name:   "rc-root",
		URL:    "",
		Method: "POST",
		Status: http.StatusNotFound,
		Expected: `{
	"error": "couldn't find method \"\"",
	"input": {},
	"path": "",
	"status": 404
}
`,
	}, {
		Name:     "rc-noop",
		URL:      "rc/noop",
		Method:   "POST",
		Status:   http.StatusOK,
		Expected: "{}\n",
	}, {
		Name:   "rc-error",
		URL:    "rc/error",
		Method: "POST",
		Status: http.StatusInternalServerError,
		Expected: `{
	"error": "arbitrary error on input map[]",
	"input": {},
	"path": "rc/error",
	"status": 500
}
`,
	}, {
		Name:     "core-gc",
		URL:      "core/gc", // returns nil, nil so check it is made into {}
		Method:   "POST",
		Status:   http.StatusOK,
		Expected: "{}\n",
	}, {
		Name:   "url-params",
		URL:    "rc/noop?param1=potato&param2=sausage",
		Method: "POST",
		Status: http.StatusOK,
		Expected: `{
	"param1": "potato",
	"param2": "sausage"
}
`,
	}, {
		Name:        "json",
		URL:         "rc/noop",
		Method:      "POST",
		Body:        `{ "param1":"string", "param2":true }`,
		ContentType: "application/json",
		Status:      http.StatusOK,
		Expected: `{
	"param1": "string",
	"param2": true
}
`,
	}, {
		Name:        "json-and-url-params",
		URL:         "rc/noop?param1=potato&param2=sausage",
		Method:      "POST",
		Body:        `{ "param1":"string", "param3":true }`,
		ContentType: "application/json",
		Status:      http.StatusOK,
		Expected: `{
	"param1": "string",
	"param2": "sausage",
	"param3": true
}
`,
	}, {
		Name:        "json-bad",
		URL:         "rc/noop?param1=potato&param2=sausage",
		Method:      "POST",
		Body:        `{ param1":"string", "param3":true }`,
		ContentType: "application/json",
		Status:      http.StatusBadRequest,
		Expected: `{
	"error": "failed to read input JSON: invalid character 'p' looking for beginning of object key string",
	"input": {
		"param1": "potato",
		"param2": "sausage"
	},
	"path": "rc/noop",
	"status": 400
}
`,
	}, {
		Name:        "form",
		URL:         "rc/noop",
		Method:      "POST",
		Body:        `param1=string&param2=true`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusOK,
		Expected: `{
	"param1": "string",
	"param2": "true"
}
`,
	}, {
		Name:        "form-and-url-params",
		URL:         "rc/noop?param1=potato&param2=sausage",
		Method:      "POST",
		Body:        `param1=string&param3=true`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusOK,
		Expected: `{
	"param1": "potato",
	"param2": "sausage",
	"param3": "true"
}
`,
	}, {
		Name:        "form-bad",
		URL:         "rc/noop?param1=potato&param2=sausage",
		Method:      "POST",
		Body:        `%zz`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusBadRequest,
		Expected: `{
	"error": "failed to parse form/URL parameters: invalid URL escape \"%zz\"",
	"input": null,
	"path": "rc/noop",
	"status": 400
}
`,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestRCWithAuth(t *testing.T) {
	tests := []testRun{{
		Name:        "core-command",
		URL:         "core/command",
		Method:      "POST",
		Body:        `command=version`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusOK,
		Expected: fmt.Sprintf(`{
	"error": false,
	"result": "rclone %s\n"
}
`, fs.Version),
	}, {
		Name:        "core-command-bad-returnType",
		URL:         "core/command",
		Method:      "POST",
		Body:        `command=version&returnType=POTATO`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusInternalServerError,
		Expected: `{
	"error": "Unknown returnType \"POTATO\"",
	"input": {
		"command": "version",
		"returnType": "POTATO"
	},
	"path": "core/command",
	"status": 500
}
`,
	}, {
		Name:        "core-command-stream",
		URL:         "core/command",
		Method:      "POST",
		Body:        `command=version&returnType=STREAM`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusOK,
		Expected: fmt.Sprintf(`rclone %s
{}
`, fs.Version),
	}, {
		Name:        "core-command-stream-error",
		URL:         "core/command",
		Method:      "POST",
		Body:        `command=unknown_command&returnType=STREAM`,
		ContentType: "application/x-www-form-urlencoded",
		Status:      http.StatusOK,
		Expected: fmt.Sprintf(`rclone %s
Unknown command
{
	"error": "exit status 1",
	"input": {
		"command": "unknown_command",
		"returnType": "STREAM"
	},
	"path": "core/command",
	"status": 500
}
`, fs.Version),
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	opt.NoAuth = true
	testServer(t, tests, &opt)
}

func TestMethods(t *testing.T) {
	tests := []testRun{{
		Name:     "options",
		URL:      "",
		Method:   "OPTIONS",
		Status:   http.StatusOK,
		Expected: "",
		Headers: map[string]string{
			"Access-Control-Allow-Origin":   "http://localhost:5572/",
			"Access-Control-Request-Method": "POST, OPTIONS, GET, HEAD",
			"Access-Control-Allow-Headers":  "authorization, Content-Type",
		},
	}, {
		Name:   "bad",
		URL:    "",
		Method: "POTATO",
		Status: http.StatusMethodNotAllowed,
		Expected: `{
	"error": "method \"POTATO\" not allowed",
	"input": null,
	"path": "",
	"status": 405
}
`,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestMetrics(t *testing.T) {
	stats := accounting.GlobalStats()
	tests := makeMetricsTestCases(stats)
	opt := newTestOpt()
	opt.EnableMetrics = true
	testServer(t, tests, &opt)

	// Test changing a couple options
	stats.Bytes(500)
	stats.Deletes(30)
	stats.Errors(2)
	stats.Bytes(324)

	tests = makeMetricsTestCases(stats)
	testServer(t, tests, &opt)
}

func makeMetricsTestCases(stats *accounting.StatsInfo) (tests []testRun) {
	tests = []testRun{{
		Name:     "Bytes Transferred Metric",
		URL:      "/metrics",
		Method:   "GET",
		Status:   http.StatusOK,
		Contains: regexp.MustCompile(fmt.Sprintf("rclone_bytes_transferred_total %d", stats.GetBytes())),
	}, {
		Name:     "Checked Files Metric",
		URL:      "/metrics",
		Method:   "GET",
		Status:   http.StatusOK,
		Contains: regexp.MustCompile(fmt.Sprintf("rclone_checked_files_total %d", stats.GetChecks())),
	}, {
		Name:     "Errors Metric",
		URL:      "/metrics",
		Method:   "GET",
		Status:   http.StatusOK,
		Contains: regexp.MustCompile(fmt.Sprintf("rclone_errors_total %d", stats.GetErrors())),
	}, {
		Name:     "Deleted Files Metric",
		URL:      "/metrics",
		Method:   "GET",
		Status:   http.StatusOK,
		Contains: regexp.MustCompile(fmt.Sprintf("rclone_files_deleted_total %d", stats.Deletes(0))),
	}, {
		Name:     "Files Transferred Metric",
		URL:      "/metrics",
		Method:   "GET",
		Status:   http.StatusOK,
		Contains: regexp.MustCompile(fmt.Sprintf("rclone_files_transferred_total %d", stats.GetTransfers())),
	},
	}
	return
}

func TestShare(t *testing.T) {
	tests := []testRun{
		{
			Name:   "share-object-randomToken-unlimited",
			URL:    "share/create",
			Method: "POST",
			Body: `{
				"fs": "testdata/files",
				"remote": "file.txt"
			}`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Contains: regexp.MustCompile(`{
	"sharedLink": "share/links/.*/file.txt"
}`),
		}, {
			Name:        "share-list",
			URL:         "share/list",
			Method:      "POST",
			Body:        `{}`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Contains: regexp.MustCompile(`{
	"sharedLinks": \[
		{
			"fs": "testdata/files",
			"remote": "file.txt",
			"sharedName": "file.txt",
			"token": ".*"
		}
	\]
}
`),
		}, {
			Name:   "share-object-not-found",
			URL:    "share/create",
			Method: "POST",
			Body: `{
				"fs": "testdata/files",
				"remote": "file2.txt"
			}`,
			ContentType: "application/json",
			Status:      http.StatusNotFound,
			Expected: `{
	"error": "object not found",
	"input": {
		"fs": "testdata/files",
		"remote": "file2.txt"
	},
	"path": "share/create",
	"status": 404
}
`,
		}, {
			Name:   "share-specify-token",
			URL:    "share/create",
			Method: "POST",
			Body: `{
				"fs": "testdata/files",
				"remote": "file.txt",
				"token": "123456"
			}`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Expected: `{
	"sharedLink": "share/links/123456/file.txt"
}
`,
		}, {
			Name:     "share-get-content",
			URL:      "share/links/123456/file.txt",
			Status:   http.StatusOK,
			Expected: "this is file1.txt\n",
			Headers: map[string]string{
				"Content-Length": "18",
			},
		}, {
			Name:     "share-get-content-noauth",
			URL:      "share/links/123456/file.txt",
			NoAuth:   true,
			Status:   http.StatusOK,
			Expected: "this is file1.txt\n",
			Headers: map[string]string{
				"Content-Length": "18",
			},
		}, {
			Name:   "share-specify-expire",
			URL:    "share/create",
			Method: "POST",
			Body: `{
					 "fs": "testdata/files",
					 "remote": "file.txt",
					 "token": "abcdef",
					 "expire": "0ms"
				 }`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Expected: `{
	"sharedLink": "share/links/abcdef/file.txt"
}
`,
		}, {
			Name:     "share-unable-access-expired-link",
			URL:      "share/links/abcdef/file.txt",
			Status:   http.StatusNotFound,
			Expected: "Not Found\n",
		}, {
			Name:   "share-delete-by-token-and-name",
			URL:    "share/delete",
			Method: "POST",
			Body: `{
					 "token": "123456",
					 "sharedName": "file.txt"
				 }`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Expected:    "{}\n",
		}, {
			Name:   "share-delete-by-fs-and-remote",
			URL:    "share/delete",
			Method: "POST",
			Body: `{
					 "fs": "testdata/files",
					 "remote": "file.txt"
				 }`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Expected:    "{}\n",
		}, {
			Name:        "share-vialidate-delete",
			URL:         "share/list",
			Method:      "POST",
			Body:        `{}`,
			ContentType: "application/json",
			Status:      http.StatusOK,
			Contains: regexp.MustCompile(`{
	"sharedLinks": \[\]
}
`),
		}, {
			Name:     "share-unable-access-deleted-link",
			URL:      "share/links/123456/file.txt",
			Status:   http.StatusNotFound,
			Expected: "Not Found\n",
		},
	}
	opt := newTestOpt()
	opt.Share = true
	opt.Files = testFs
	opt.HTTPOptions.BasicUser = "user"
	opt.HTTPOptions.BasicPass = "pass"
	testServerWithListen(t, tests, &opt, true)
}

var matchRemoteDirListing = regexp.MustCompile(`<title>Directory listing of /</title>`)

func TestServingRoot(t *testing.T) {
	tests := []testRun{{
		Name:     "rootlist",
		URL:      "*",
		Status:   http.StatusOK,
		Contains: matchRemoteDirListing,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestServingRootNoFiles(t *testing.T) {
	tests := []testRun{{
		Name:     "rootlist",
		URL:      "",
		Status:   http.StatusOK,
		Contains: matchRemoteDirListing,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = ""
	testServer(t, tests, &opt)
}

func TestNoFiles(t *testing.T) {
	tests := []testRun{{
		Name:     "file",
		URL:      "file.txt",
		Status:   http.StatusNotFound,
		Expected: "Not Found\n",
	}, {
		Name:     "dir",
		URL:      "dir/",
		Status:   http.StatusNotFound,
		Expected: "Not Found\n",
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = ""
	testServer(t, tests, &opt)
}

func TestNoServe(t *testing.T) {
	tests := []testRun{{
		Name:     "file",
		URL:      remoteURL + "file.txt",
		Status:   http.StatusNotFound,
		Expected: "404 page not found\n",
	}, {
		Name:     "dir",
		URL:      remoteURL + "dir/",
		Status:   http.StatusNotFound,
		Expected: "404 page not found\n",
	}}
	opt := newTestOpt()
	opt.Serve = false
	opt.Files = testFs
	testServer(t, tests, &opt)
}

func TestAuthRequired(t *testing.T) {
	tests := []testRun{{
		Name:        "auth",
		URL:         "rc/noopauth",
		Method:      "POST",
		Body:        `{}`,
		ContentType: "application/javascript",
		Status:      http.StatusForbidden,
		Expected: `{
	"error": "authentication must be set up on the rc server to use \"rc/noopauth\" or the --rc-no-auth flag must be in use",
	"input": {},
	"path": "rc/noopauth",
	"status": 403
}
`,
	}}
	opt := newTestOpt()
	opt.Serve = false
	opt.Files = ""
	opt.NoAuth = false
	testServer(t, tests, &opt)
}

func TestNoAuth(t *testing.T) {
	tests := []testRun{{
		Name:        "auth",
		URL:         "rc/noopauth",
		Method:      "POST",
		Body:        `{}`,
		ContentType: "application/javascript",
		Status:      http.StatusOK,
		Expected:    "{}\n",
	}}
	opt := newTestOpt()
	opt.Serve = false
	opt.Files = ""
	opt.NoAuth = true
	testServer(t, tests, &opt)
}

func TestWithUserPass(t *testing.T) {
	tests := []testRun{{
		Name:        "auth",
		URL:         "rc/noopauth",
		Method:      "POST",
		Body:        `{}`,
		ContentType: "application/javascript",
		Status:      http.StatusOK,
		Expected:    "{}\n",
	}}
	opt := newTestOpt()
	opt.Serve = false
	opt.Files = ""
	opt.NoAuth = false
	opt.HTTPOptions.BasicUser = "user"
	opt.HTTPOptions.BasicPass = "pass"
	testServer(t, tests, &opt)
}

func TestRCAsync(t *testing.T) {
	tests := []testRun{{
		Name:        "ok",
		URL:         "rc/noop",
		Method:      "POST",
		ContentType: "application/json",
		Body:        `{ "_async":true }`,
		Status:      http.StatusOK,
		Contains:    regexp.MustCompile(`(?s)\{.*\"jobid\":.*\}`),
	}, {
		Name:        "bad",
		URL:         "rc/noop",
		Method:      "POST",
		ContentType: "application/json",
		Body:        `{ "_async":"truthy" }`,
		Status:      http.StatusBadRequest,
		Expected: `{
	"error": "couldn't parse key \"_async\" (truthy) as bool: strconv.ParseBool: parsing \"truthy\": invalid syntax",
	"input": {
		"_async": "truthy"
	},
	"path": "rc/noop",
	"status": 400
}
`,
	}}
	opt := newTestOpt()
	opt.Serve = true
	opt.Files = ""
	testServer(t, tests, &opt)
}
