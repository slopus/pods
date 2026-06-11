package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/slopus/pods/internal/api"
	"github.com/slopus/pods/internal/client"
)

// podsManifest is the pods.json file at the root of a site directory.
type podsManifest struct {
	Name string `json:"name"`
}

func cmdLogin(args []string) error {
	fs, endpoint, secret := newFlagSet("login")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: pods login [--endpoint URL] [--token T]")
	}

	ep, sec := *endpoint, *secret
	if ep == "" {
		ep = defaultEndpoint
	}
	ep = normalizeEndpoint(ep)
	if sec == "" {
		return cmdGitHubLogin(ep)
	}
	c := client.New(ep, sec)
	me, err := c.Me(context.Background())
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	if !me.Authenticated || me.User == nil {
		return errors.New("login failed: token was not accepted")
	}

	path, err := configPath()
	if err != nil {
		return err
	}
	if err := saveConfigFile(path, config{Endpoint: c.Endpoint(), Secret: sec}); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as %s\n", c.Endpoint(), me.User.ID)
	return nil
}

func cmdGitHubLogin(endpoint string) error {
	c := client.New(endpoint, "")
	start, err := c.GitHubDeviceStart(context.Background())
	if err != nil {
		return fmt.Errorf("GitHub login failed to start: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Open %s and enter code %s\n", start.VerificationURI, start.UserCode)
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", start.VerificationURI).Start()
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for {
		if time.Now().After(deadline) {
			return errors.New("GitHub login expired")
		}
		time.Sleep(interval)
		res, err := c.GitHubDevicePoll(context.Background(), start.DeviceCode)
		if err != nil {
			// A 401 is a definitive denial; anything else (a 5xx or a
			// network blip) is transient — keep polling until the deadline.
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 401 {
				return fmt.Errorf("GitHub login failed: %w", err)
			}
			continue
		}
		if res.Pending {
			if res.Interval > 0 {
				interval = time.Duration(res.Interval) * time.Second
			}
			continue
		}
		if res.Token == "" || res.User == nil {
			return errors.New("GitHub login did not return an API token")
		}
		path, err := configPath()
		if err != nil {
			return err
		}
		if err := saveConfigFile(path, config{Endpoint: c.Endpoint(), Secret: res.Token}); err != nil {
			return err
		}
		userID := res.User.Login
		if userID == "" {
			userID = res.User.ID
		}
		fmt.Printf("logged in to %s as %s\n", c.Endpoint(), userID)
		return nil
	}
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("pods logout", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("logged out")
	return nil
}

func cmdStatus(args []string) error {
	fs, endpoint, secret := newFlagSet("status")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}
	ctx := context.Background()

	health := "ok"
	if h, err := c.Health(ctx); err != nil {
		health = "unreachable (" + err.Error() + ")"
	} else if !h.OK {
		health = "not ok"
	}
	fmt.Printf("endpoint:    %s\n", c.Endpoint())
	fmt.Printf("health:      %s\n", health)
	if me, err := c.Me(ctx); err == nil && me.Authenticated && me.User != nil {
		fmt.Printf("user:        %s\n", me.User.ID)
	} else if err == nil {
		fmt.Printf("user:        anonymous\n")
	} else {
		fmt.Printf("user:        unavailable (%v)\n", err)
	}

	sites, err := c.Sites(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("sites:       %d\n", len(sites))
	if colls, err := c.Collections(ctx); err == nil {
		fmt.Printf("collections: %d\n", len(colls))
	} else {
		fmt.Printf("collections: unavailable (%v)\n", err)
	}
	return nil
}

const starterIndexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Happy Pods</title>
  <style>
    :root { color-scheme: light dark; }
    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: linear-gradient(160deg, #1d2b53 0%%, #7e2553 100%%);
      color: #fff;
    }
    main { text-align: center; padding: 2rem; max-width: 36rem; }
    h1 { font-size: 3rem; margin: 0 0 0.5rem; }
    p { opacity: 0.85; line-height: 1.6; }
    code {
      background: rgba(255, 255, 255, 0.15);
      padding: 0.15em 0.4em;
      border-radius: 4px;
    }
  </style>
</head>
<body>
  <main>
    <h1>Happy Pods</h1>
    <p>Your pod is live. Edit <code>index.html</code> and run
    <code>pods deploy</code> to ship a new version.</p>
  </main>

  <!--
    Need a database? Happy Pods gives every site its own JSON store with a
    zero-dependency browser client. It is public on your site's host, so no
    token is needed from the page. Uncomment:

    <script src="/pods.js"></script>
    <script type="module">
      const pods = Pods(); // same-origin, scoped to this site
      const posts = pods.db.collection("posts");
      await posts.create({ title: "hello, pods" });
      const { docs } = await posts.query({ sort: "-created_at", limit: 10 });
      console.log(docs);
    </script>
  -->
</body>
</html>
`

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("pods init", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("usage: pods init [dir]")
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	manifest, err := json.Marshal(podsManifest{Name: filepath.Base(abs)})
	if err != nil {
		return err
	}
	writes := []struct {
		name string
		data []byte
	}{
		{"pods.json", append(manifest, '\n')},
		{"index.html", []byte(strings.ReplaceAll(starterIndexHTML, "%%", "%"))},
	}
	for _, w := range writes {
		path := filepath.Join(dir, w.name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("initialized %s\n", dir)
	fmt.Println("  pods.json")
	fmt.Println("  index.html")
	fmt.Printf("run \"pods deploy %s\" to ship it\n", dir)
	return nil
}

func cmdDeploy(args []string) error {
	fs, endpoint, secret := newFlagSet("deploy")
	name := fs.String("name", "", "site name (defaults to pods.json, then the directory name)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Allow "pods deploy <dir> --name foo": re-parse flags that follow the
	// positional directory argument.
	rest := fs.Args()
	dir := "."
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dir = rest[0]
		if err := parseFlags(fs, rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	if len(rest) > 0 {
		return errors.New("usage: pods deploy [dir] [--name N]")
	}

	siteName, err := resolveSiteName(*name, dir)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	files, _, err := packageDir(&buf, dir)
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("nothing to deploy in %s", dir)
	}

	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}
	res, err := c.Deploy(context.Background(), siteName, &buf)
	if err != nil {
		return err
	}
	fmt.Printf("deployed %q: %d files, %s\n", res.Site.Name, res.Site.Files, humanBytes(res.Site.Bytes))
	url := res.URL
	if url == "" {
		url = c.SubdomainSiteURL(siteName)
	}
	fmt.Println(url)
	return nil
}

// resolveSiteName picks the site name: --name flag, then the "name" field of
// <dir>/pods.json, then the directory's base name.
func resolveSiteName(flagName, dir string) (string, error) {
	if flagName != "" {
		return flagName, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "pods.json"))
	switch {
	case err == nil:
		var m podsManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return "", fmt.Errorf("parsing %s: %w", filepath.Join(dir, "pods.json"), err)
		}
		if m.Name != "" {
			return m.Name, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Base(abs), nil
}

func cmdList(args []string) error {
	fs, endpoint, secret := newFlagSet("list")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}
	sites, err := c.Sites(context.Background())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tOWNER\tFILES\tSIZE\tUPDATED")
	for _, s := range sites {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			s.Name, s.OwnerLogin, s.Files, humanBytes(s.Bytes), s.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func cmdRm(args []string) error {
	fs, endpoint, secret := newFlagSet("rm")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Allow "pods rm <site> --yes": re-parse flags that follow the
	// positional site argument.
	rest := fs.Args()
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		if err := parseFlags(fs, rest[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: pods rm <site> [--yes]")
		}
	} else if len(rest) != 1 {
		return errors.New("usage: pods rm <site> [--yes]")
	}
	site := rest[0]
	if !*yes {
		ok, err := confirm(fmt.Sprintf("delete site %q? [y/N] ", site))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted")
			return nil
		}
	}
	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}
	if err := c.DeleteSite(context.Background(), site); err != nil {
		return err
	}
	fmt.Printf("deleted site %q\n", site)
	return nil
}

func cmdOpen(args []string) error {
	fs, endpoint, secret := newFlagSet("open")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pods open <site>")
	}
	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}
	url := c.SubdomainSiteURL(fs.Arg(0))
	fmt.Println(url)
	if runtime.GOOS == "darwin" {
		// Best effort; the URL is already printed.
		_ = exec.Command("open", url).Start()
	}
	return nil
}

type stringList []string

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func cmdDB(args []string) error {
	fs, endpoint, secret := newFlagSet("db")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: pods db <coll> <list|get|create|set|patch|rm|drop> [arguments]")
	}

	coll := fs.Arg(0)
	action := fs.Arg(1)
	actionArgs := fs.Args()[2:]
	switch action {
	case "list", "get", "create", "set", "patch", "rm", "drop":
	default:
		return fmt.Errorf("unknown db command %q", action)
	}

	c, err := apiClient(*endpoint, *secret)
	if err != nil {
		return err
	}

	switch action {
	case "list":
		return cmdDBList(c, coll, actionArgs)
	case "get":
		return cmdDBGet(c, coll, actionArgs)
	case "create":
		return cmdDBCreate(c, coll, actionArgs)
	case "set":
		return cmdDBSet(c, coll, actionArgs)
	case "patch":
		return cmdDBPatch(c, coll, actionArgs)
	case "rm":
		return cmdDBRm(c, coll, actionArgs)
	case "drop":
		return cmdDBDrop(c, coll, actionArgs)
	default:
		return fmt.Errorf("unknown db command %q", action)
	}
}

func cmdDBList(c *client.Client, coll string, args []string) error {
	fs := flag.NewFlagSet("pods db "+coll+" list", flag.ContinueOnError)
	var where stringList
	fs.Var(&where, "where", "top-level equality filter field=value (repeatable)")
	sortBy := fs.String("sort", "", "sort field, or -field for descending")
	limit := fs.Int("limit", 0, "maximum documents to return (0 means no limit)")
	offset := fs.Int("offset", 0, "documents to skip")
	jsonOut := fs.Bool("json", false, "print the full query result as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: pods db <coll> list [--where k=v]... [--sort f] [--limit n] [--offset n] [--json]")
	}
	if *limit < 0 || *offset < 0 {
		return errors.New("limit and offset must be non-negative")
	}

	res, err := c.Query(context.Background(), coll, client.QueryOptions{
		Where:  []string(where),
		Sort:   *sortBy,
		Limit:  *limit,
		Offset: *offset,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(res, false)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, doc := range res.Docs {
		if err := enc.Encode(doc); err != nil {
			return err
		}
	}
	return nil
}

func cmdDBGet(c *client.Client, coll string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pods db <coll> get <id>")
	}
	doc, err := c.GetDoc(context.Background(), coll, args[0])
	if err != nil {
		return err
	}
	return printJSON(doc, true)
}

func cmdDBCreate(c *client.Client, coll string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pods db <coll> create <json|->")
	}
	doc, err := readDocArg(args[0])
	if err != nil {
		return err
	}
	created, err := c.CreateDoc(context.Background(), coll, doc)
	if err != nil {
		return err
	}
	return printJSON(created, true)
}

func cmdDBSet(c *client.Client, coll string, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: pods db <coll> set <id> <json|->")
	}
	doc, err := readDocArg(args[1])
	if err != nil {
		return err
	}
	set, err := c.SetDoc(context.Background(), coll, args[0], doc)
	if err != nil {
		return err
	}
	return printJSON(set, true)
}

func cmdDBPatch(c *client.Client, coll string, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: pods db <coll> patch <id> <json|->")
	}
	doc, err := readDocArg(args[1])
	if err != nil {
		return err
	}
	patched, err := c.PatchDoc(context.Background(), coll, args[0], doc)
	if err != nil {
		return err
	}
	return printJSON(patched, true)
}

func cmdDBRm(c *client.Client, coll string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pods db <coll> rm <id>")
	}
	if err := c.DeleteDoc(context.Background(), coll, args[0]); err != nil {
		return err
	}
	fmt.Printf("deleted document %q from %q\n", args[0], coll)
	return nil
}

func cmdDBDrop(c *client.Client, coll string, args []string) error {
	fs := flag.NewFlagSet("pods db "+coll+" drop", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: pods db <coll> drop [--yes]")
	}
	if !*yes {
		ok, err := confirm(fmt.Sprintf("drop collection %q? [y/N] ", coll))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted")
			return nil
		}
	}
	if err := c.DropCollection(context.Background(), coll); err != nil {
		return err
	}
	fmt.Printf("dropped collection %q\n", coll)
	return nil
}

func readDocArg(src string) (api.Doc, error) {
	var data []byte
	var err error
	if src == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data = []byte(src)
	}
	if err != nil {
		return nil, err
	}
	var doc api.Doc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing JSON document: %w", err)
	}
	if doc == nil {
		return nil, errors.New("JSON document must be an object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("JSON document must contain a single object")
		}
		return nil, fmt.Errorf("parsing JSON document: %w", err)
	}
	return doc, nil
}

func printJSON(v any, pretty bool) error {
	enc := json.NewEncoder(os.Stdout)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// normalizeEndpoint trims whitespace and trailing slashes and defaults the
// scheme to http:// when none is given.
func normalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return strings.TrimRight(s, "/")
}

// readLine reads one line from r, trimming whitespace. EOF with content is
// not an error; EOF without content is.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" && errors.Is(err, io.EOF) {
		return "", io.ErrUnexpectedEOF
	}
	return line, nil
}

// confirm prints prompt to stderr and reads a y/N answer from stdin.
func confirm(prompt string) (bool, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
