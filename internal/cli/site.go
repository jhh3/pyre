package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/site"
)

// serveSite runs the review site (tests replace it).
var serveSite = func(ctx context.Context, opts site.Options) error {
	s, err := site.New(opts)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// siteRoot is the page folder: $PYRE_SITE_DIR, else
// $XDG_DATA_HOME/pyre/site/pages, else ~/.local/share/pyre/site/pages.
func (a *App) siteRoot() string {
	if d := a.Getenv("PYRE_SITE_DIR"); d != "" {
		return d
	}
	if x := a.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "pyre", "site", "pages")
	}
	return filepath.Join(a.Getenv("HOME"), ".local", "share", "pyre", "site", "pages")
}

// siteStateDir holds notes.json: $XDG_STATE_HOME/pyre/site, else
// ~/.local/state/pyre/site.
func (a *App) siteStateDir() string {
	if x := a.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pyre", "site")
	}
	return filepath.Join(a.Getenv("HOME"), ".local", "state", "pyre", "site")
}

// addSiteCommands adds `pyre site` (the review site, docs/site.md).
func (a *App) addSiteCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "site", Short: "The review site: pages for people to read and leave notes on (docs/site.md)"}
	var dir string
	group.PersistentFlags().StringVar(&dir, "dir", "", "page folder (default $PYRE_SITE_DIR, else ~/.local/share/pyre/site/pages)")
	pageRoot := func() string {
		if dir != "" {
			return dir
		}
		return a.siteRoot()
	}

	// new
	var meta site.Meta
	var noSession bool
	newCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a page folder (page.json, a starter index.html, assets/)",
		Long: `Create <dir>/<name>/ with page.json and a starter index.html. The name is
the page's URL (/p/<name>/) and part of every note's key, so pick it once.

The page's session defaults to $PYRE_SESSION_ID: notes people leave on the
page are delivered to that session as Pyre messages. --no-session leaves it
unset.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if !site.ValidSlug(args[0]) {
				return usagef("%q is not a page name: use lowercase letters, digits and hyphens", args[0])
			}
			if meta.Session == "" && !noSession {
				meta.Session = a.Getenv("PYRE_SESSION_ID")
			}
			d, err := site.Scaffold(pageRoot(), args[0], meta, time.Now())
			if err != nil {
				return c.Errorf(c.ErrConflict, "%v", err)
			}
			return a.emit(map[string]string{"page": args[0], "dir": d, "session": meta.Session}, func(w io.Writer) {
				fmt.Fprintf(w, "Created %s\n  edit %s and %s; put images in assets/\n", d, filepath.Join(d, site.HTMLFile), filepath.Join(d, site.PageFile))
				if meta.Session != "" {
					fmt.Fprintf(w, "  new notes on it are sent to %s\n", meta.Session)
				}
				fmt.Fprintf(w, "  check it: pyre site build\n")
			})
		}),
	}
	nf := newCmd.Flags()
	nf.StringVar(&meta.Title, "title", "", "page title (default: the name)")
	nf.StringVar(&meta.Summary, "summary", "", "one line for the index card")
	nf.StringVar(&meta.Owner, "owner", "", "who to ask about the page (free text)")
	nf.StringVar(&meta.Session, "session", "", "Pyre session that receives new notes (default $PYRE_SESSION_ID)")
	nf.BoolVar(&noSession, "no-session", false, "do not send notes to any session")
	group.AddCommand(newCmd)

	// build
	var out, title string
	build := &cobra.Command{
		Use:   "build",
		Short: "Check every page (page.json, index.html, data-note IDs); --out also writes a static copy",
		Long: `Check every page folder: page.json parses and has a title, index.html exists
and has a <body>, and each data-note ID is well formed and used once. The
server renders pages on every request, so a change is live as soon as the
file is saved; build is the check (and, with --out, a static export).`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			var pages []site.Page
			var probs []site.Problem
			var err error
			if out != "" {
				pages, probs, err = site.Export(pageRoot(), out, title, time.Now())
			} else {
				pages, probs, err = site.Check(pageRoot())
			}
			if err != nil {
				return err
			}
			type row struct {
				Page    string `json:"page"`
				Title   string `json:"title"`
				Status  string `json:"status"`
				Session string `json:"session,omitempty"`
			}
			res := struct {
				Root     string         `json:"root"`
				Pages    []row          `json:"pages"`
				Problems []site.Problem `json:"problems"`
			}{Root: pageRoot(), Pages: []row{}, Problems: probs}
			if res.Problems == nil {
				res.Problems = []site.Problem{}
			}
			for _, p := range pages {
				res.Pages = append(res.Pages, row{p.Slug, p.Meta.Title, p.Meta.Status, p.Meta.Session})
			}
			if err := a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "%d pages in %s\n", len(pages), res.Root)
				for _, r := range res.Pages {
					fmt.Fprintf(w, "  /p/%s/  %s  [%s]\n", r.Page, r.Title, firstNonEmptyStr(r.Status, "draft"))
				}
				for _, p := range probs {
					fmt.Fprintf(w, "problem: %s\n", p)
				}
				if out != "" {
					fmt.Fprintf(w, "static copy: %s\n", out)
				}
			}); err != nil {
				return err
			}
			if len(probs) > 0 {
				return c.Errorf(c.ErrInvalid, "%d problem(s) in the pages", len(probs))
			}
			return nil
		}),
	}
	build.Flags().StringVar(&out, "out", "", "also write a static copy of the site here")
	build.Flags().StringVar(&title, "title", site.DefaultTitle, "site title")
	group.AddCommand(build)

	// serve
	var listen, stateDir string
	var domains, emails, origins []string
	var dev, noDeliver bool
	var stitle string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the review site on loopback (a signing-in proxy such as ngrok puts it online)",
		Long: `Serve the pages and the notes API on loopback (default ` + site.DefaultListen + `).

Every request needs an X-Pyre-Email header (set by the ngrok policy from the
Google sign-in) whose address is listed with --allow-email or whose domain is
listed with --allow-domain; --dev also admits loopback requests without it,
for local testing (never behind the tunnel). Writes must be JSON from the
site's own origin.

A new note on a page whose page.json names a session is sent to that session
as a Pyre message, using this client's credentials (pyre login); without a
hub connection notes are still kept. See docs/site.md.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			if err := site.CheckListen(listen); err != nil {
				return usagef("%v", err)
			}
			if !dev && len(domains) == 0 && len(emails) == 0 {
				return usagef("say who may use the site: --allow-domain example.org and/or --allow-email someone@example.org (or --dev for local testing)")
			}
			if stateDir == "" {
				stateDir = a.siteStateDir()
			}
			var deliver site.Deliver
			if !noDeliver {
				ops, err := a.connect()
				if err != nil {
					fmt.Fprintf(a.Stderr, "pyre site: no hub connection (%v); notes are kept but not sent to sessions\n", err)
				} else {
					deliver = func(ctx context.Context, session, text string) (string, error) {
						mv, err := ops.API.SendMessage(ctx, c.SendMessageRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
							Recipient: c.Address{Kind: c.AddrSession, ID: session}, Body: c.MessageBody{Text: text}})
						if err != nil {
							return "", err
						}
						return mv.Envelope.MessageID, nil
					}
				}
			}
			note := ""
			if dev {
				note = " [dev: loopback requests without sign-in are allowed]"
			}
			fmt.Fprintf(a.Stderr, "pyre site: serving %s on http://%s (notes in %s)%s\n", pageRoot(), listen, stateDir, note)
			return serveSite(cmd.Context(), site.Options{Root: pageRoot(), StateDir: stateDir, Listen: listen, Title: stitle,
				AllowDomains: domains, AllowEmails: emails, Origins: origins, Dev: dev, Deliver: deliver})
		}),
	}
	sf := serve.Flags()
	sf.StringVar(&listen, "listen", site.DefaultListen, "loopback address to listen on")
	sf.StringVar(&stateDir, "state-dir", "", "where notes.json lives (default $XDG_STATE_HOME/pyre/site or ~/.local/state/pyre/site)")
	sf.StringSliceVar(&domains, "allow-domain", nil, "email domain allowed in (repeatable; exact match, not subdomains)")
	sf.StringSliceVar(&emails, "allow-email", nil, "single email address allowed in (repeatable)")
	sf.StringSliceVar(&origins, "origin", nil, "public origin(s) accepted on writes; the first is the base of links in delivered notes")
	sf.StringVar(&stitle, "title", site.DefaultTitle, "site title")
	sf.BoolVar(&dev, "dev", false, "admit loopback requests without X-Pyre-Email (local testing only)")
	sf.BoolVar(&noDeliver, "no-deliver", false, "keep notes but never send them to sessions")
	group.AddCommand(serve)

	// notes
	var since time.Duration
	notes := &cobra.Command{
		Use:   "notes [page]",
		Short: "Print the notes people left (all pages, or one), oldest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			page := ""
			if len(args) == 1 {
				page = args[0]
				if !site.ValidSlug(page) {
					return usagef("%q is not a page name", page)
				}
			}
			if stateDir == "" {
				stateDir = a.siteStateDir()
			}
			db, err := site.ReadNotes(stateDir)
			if err != nil {
				return err
			}
			var from time.Time
			if since > 0 {
				from = time.Now().Add(-since)
			}
			list := site.List(db, page, from)
			return a.emit(map[string]any{"notes": list}, func(w io.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(w, "No notes.")
					return
				}
				for _, n := range list {
					item := n.Item
					if item == site.PageItem {
						item = "(whole page)"
					}
					who := n.Author
					if n.Name != "" {
						who = n.Name + " <" + n.Author + ">"
					}
					edited := ""
					if n.Edited != nil {
						edited = " (edited)"
					}
					fmt.Fprintf(w, "%s / %s · %s · %s%s\n", n.Page, item, who, site.FormatTime(n.Created), edited)
					for _, l := range strings.Split(n.Text, "\n") {
						fmt.Fprintf(w, "    %s\n", l)
					}
				}
			})
		}),
	}
	notes.Flags().DurationVar(&since, "since", 0, "only notes newer than this (e.g. 24h)")
	notes.Flags().StringVar(&stateDir, "state-dir", "", "where notes.json lives (default $XDG_STATE_HOME/pyre/site or ~/.local/state/pyre/site)")
	group.AddCommand(notes)

	root.AddCommand(group)
}
