// Command prreport writes an HTML report of what a pull request changes in a
// Go module: functions, state reads and writes, packages, and go.mod
// dependencies.
//
//	prreport -repo ~/projects/mediamtx -pr 6427
//	prreport -repo ~/projects/mediamtx -base origin/main -head my-branch
//
// The base revision is the merge base of -base and -head, as on GitHub.
// Both revisions are exported with git archive into a temporary directory,
// so the repository's working tree is untouched.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed report.html
var reportHTML string

func main() {
	repo := flag.String("repo", ".", "git repository")
	dir := flag.String("dir", ".", "module directory within the repository")
	base := flag.String("base", "", "base branch or revision (default: the remote's default branch, e.g. origin/main)")
	head := flag.String("head", "HEAD", "head branch or revision")
	pr := flag.Int("pr", 0, "GitHub pull request number to fetch from origin as the head")
	out := flag.String("out", "prreport.html", "output HTML file")
	tests := flag.Bool("tests", false, "include _test.go files")
	gh := flag.String("github", "", "GitHub repository URL for source links (default: derived from origin)")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", *repo}, args...)...)
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			log.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(b))
	}

	if *base == "" {
		*base = defaultBranch(*repo)
	}
	headRef := *head
	if *pr > 0 {
		log.Printf("fetching pull request #%d from origin", *pr)
		git("fetch", "--quiet", "origin", fmt.Sprintf("pull/%d/head", *pr))
		*head = git("rev-parse", "FETCH_HEAD")
		headRef = fmt.Sprintf("#%d", *pr)
	}
	headSHA := git("rev-parse", "--verify", *head+"^{commit}")
	baseSHA := git("merge-base", *base, headSHA)
	if *gh == "" {
		*gh = githubURL(git("remote", "get-url", "origin"))
	}

	tmp, err := os.MkdirTemp("", "prreport")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	snaps := map[string]*Snapshot{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, sha := range map[string]string{"base": baseSHA, "head": headSHA} {
		root := filepath.Join(tmp, name)
		if err := export(*repo, sha, root); err != nil {
			log.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			s, err := takeSnapshot(filepath.Join(root, *dir), sha, patterns, *tests)
			if err != nil {
				log.Fatalf("loading %s (%s): %v", name, sha[:10], err)
			}
			log.Printf("loaded %s %s: %d functions, %d packages in %s", name, sha[:10], len(s.Funcs), len(s.Packages), time.Since(start).Round(time.Millisecond))
			mu.Lock()
			snaps[name] = s
			mu.Unlock()
		}()
	}
	wg.Wait()

	d := &differ{base: snaps["base"], head: snaps["head"], dir: filepath.ToSlash(filepath.Clean(*dir)), gh: *gh}
	rev := func(ref, sha string) Rev {
		r := Rev{Ref: ref, SHA: sha}
		if *gh != "" {
			r.URL = *gh + "/tree/" + sha
		}
		return r
	}
	rep := Report{
		Title:    fmt.Sprintf("%s → %s", *base, headRef),
		Base:     rev(*base, baseSHA),
		Head:     rev(headRef, headSHA),
		Sections: d.report(),
	}
	for _, e := range d.head.Errors {
		rep.Errors = append(rep.Errors, "head: "+e)
	}
	for _, e := range d.base.Errors {
		rep.Errors = append(rep.Errors, "base: "+e)
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := template.Must(template.New("report").Funcs(template.FuncMap{"split": strings.Fields}).Parse(reportHTML))
	if err := tmpl.Execute(f, rep); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	for _, s := range rep.Sections {
		log.Printf("%-30s +%d −%d ~%d", s.Title, s.Count(added), s.Count(removed), s.Count(modified)+s.Count(moved))
	}
	abs, _ := filepath.Abs(*out)
	log.Printf("wrote %s", abs)
}

// export writes the tree at sha into dir.
func export(repo, sha, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive := exec.Command("git", "-C", repo, "archive", "--format=tar", sha)
	untar := exec.Command("tar", "-x", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	archive.Stderr, untar.Stderr = os.Stderr, os.Stderr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		return fmt.Errorf("git archive %s: %w", sha, err)
	}
	return untar.Wait()
}

// defaultBranch returns the remote-tracking default branch, e.g. "origin/main".
func defaultBranch(repo string) string {
	b, err := exec.Command("git", "-C", repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return "origin/main"
	}
	return strings.TrimSpace(string(b))
}

var githubRemote = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?/?$`)

// githubURL converts a GitHub remote URL (https or ssh) to a web URL.
func githubURL(remote string) string {
	m := githubRemote.FindStringSubmatch(remote)
	if m == nil {
		return ""
	}
	return "https://github.com/" + m[1] + "/" + m[2]
}
