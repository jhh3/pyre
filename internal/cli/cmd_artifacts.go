package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/artifacts/cache"
	c "github.com/jhh3/pyre/internal/contracts"
)

func (a *App) addArtifactCommands(root *cobra.Command) {
	var pp PublishParams
	publish := &cobra.Command{
		Use:   "publish <file> [--title T] [--project P] [--artifact ID] [--review]",
		Short: "Publish a file as an immutable, durable artifact version",
		Long: `Upload a file to the hub as a new immutable artifact version. Success is
reported only after the hub has verified the bytes (size and sha256) and the
receipt matches the local file; a failed or mismatched upload is never
reported as published. The local file is not itself shared: reference the
printed version ID (in messages, comments, handoffs).`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := pp
			p.Path, p.OperationID = args[0], a.opID
			rec, err := ops.Publish(cmd.Context(), p)
			if err != nil {
				a.warnf("not published: %s remains a local file only", args[0])
				return err
			}
			return a.emit(rec, func(w io.Writer) {
				v := rec.Version
				fmt.Fprintf(w, "published %q: artifact %s version %s (seq %d)\n", rec.Artifact.Title, rec.Artifact.ArtifactID, v.VersionID, v.Seq)
				fmt.Fprintf(w, "  %s, %d bytes, %s, filename %s\n", v.Digest, v.Size, v.MediaType, v.Filename)
				fmt.Fprintf(w, "  durable on the hub; retrievable even if this machine is offline: pyre artifact get %s\n", v.VersionID)
				fmt.Fprintf(w, "  the local file %s is not shared; reference %s instead\n", args[0], v.VersionID)
				if v.ReviewRequested {
					fmt.Fprintln(w, "  review requested from the owner")
				}
				if v.PreviewURL != "" {
					fmt.Fprintf(w, "  preview (isolated): %s\n", v.PreviewURL)
				}
			})
		}),
	}
	pf := publish.Flags()
	pf.StringVar(&pp.Title, "title", "", "artifact title (default: file name)")
	pf.StringVar(&pp.Description, "description", "", "artifact description")
	pf.StringVar(&pp.Project, "project", "", "project ID or name")
	pf.StringVar(&pp.ArtifactID, "artifact", "", "publish a new version of this artifact")
	pf.BoolVar(&pp.Review, "review", false, "ask the owner to review this version")
	pf.StringVar(&pp.Note, "note", "", "version note")
	pf.StringVar(&pp.MediaType, "media-type", "", "media type (default: detected)")
	root.AddCommand(publish)

	art := &cobra.Command{Use: "artifact", Aliases: []string{"artifacts"}, Short: "Published artifacts and their immutable versions", Args: cobra.NoArgs}
	var af ArtifactFilter
	artLs := &cobra.Command{
		Use:   "ls [--project P] [--session S]",
		Short: "List artifacts",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			page, err := ops.ListArtifacts(cmd.Context(), af)
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, x := range page.Items {
					rows = append(rows, []string{x.ArtifactID, clip(x.Title, 40), fmt.Sprint(x.VersionCount), orDash(x.LatestVersionID), strPtr(x.ProjectID), orDash(x.SessionID), ago(x.UpdatedAt)})
				}
				table(w, "ID\tTITLE\tVERSIONS\tLATEST\tPROJECT\tSESSION\tUPDATED", rows)
			})
		}),
	}
	artLs.Flags().StringVar(&af.Project, "project", "", "project ID or name")
	artLs.Flags().StringVar(&af.Session, "session", "", "producing session")
	art.AddCommand(artLs)

	art.AddCommand(&cobra.Command{
		Use:   "versions <artifact_id|title>",
		Short: "List an artifact's versions",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			aid, err := ops.artifactID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.ArtifactVersion], error) {
				return ops.API.ListVersions(ctx, c.ListVersionsRequest{PageRequest: c.PageRequest{Cursor: cur}, ArtifactID: aid})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, v := range page.Items {
					review := ""
					if v.ReviewRequested {
						review = "review"
					}
					rows = append(rows, []string{fmt.Sprint(v.Seq), v.VersionID, v.Filename, v.MediaType, fmt.Sprint(v.Size), principalText(v.Author), orDash(v.Provenance.SessionID), ago(v.CreatedAt), review})
				}
				table(w, "SEQ\tVERSION\tFILENAME\tTYPE\tBYTES\tAUTHOR\tSESSION\tAGE\t", rows)
			})
		}),
	})

	art.AddCommand(&cobra.Command{
		Use:   "show <version_id|artifact_id|title>",
		Short: "Show a version's manifest and provenance (an artifact means its latest version)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			vid, err := ops.ResolveVersionID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			v, err := ops.API.GetVersion(cmd.Context(), c.GetRequest{ID: vid})
			if err != nil {
				return err
			}
			return a.emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "%s  artifact %s seq %d  %s\n", v.VersionID, v.ArtifactID, v.Seq, v.Filename)
				fmt.Fprintf(w, "  %s, %d bytes, %s\n", v.Digest, v.Size, v.MediaType)
				fmt.Fprintf(w, "  by %s %s; session %s run %s node %s project %s\n", principalText(v.Author), ago(v.CreatedAt), orDash(v.Provenance.SessionID), orDash(v.Provenance.RunID), orDash(v.Provenance.NodeID), strPtr(v.Provenance.ProjectID))
				if v.PreviousVersionID != "" {
					fmt.Fprintf(w, "  previous version %s\n", v.PreviousVersionID)
				}
				if v.Note != "" {
					fmt.Fprintf(w, "  note: %s\n", v.Note)
				}
			})
		}),
	})

	var out string
	var noCache bool
	var stall time.Duration
	get := &cobra.Command{
		Use:   "get <version_id|artifact_id|title> [-o FILE|-] [--stall-timeout 60s]",
		Short: "Download a version's exact bytes (digest-verified; verified local cache when offline)",
		Long: `Download a version's exact bytes, verified against its size and sha256.

Progress is printed to stderr (at most every 5s when stderr is not a
terminal). If no bytes arrive for --stall-timeout, the attempt is aborted
and retried (up to 3 times, with backoff), resuming with HTTP Range from
<file>.part next to the destination; a failed get keeps <file>.part, and
rerunning the same command resumes from it. There is no overall deadline.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			vid, err := ops.ResolveVersionID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.artifactGet(cmd.Context(), ops, vid, out, noCache, stall)
		}),
	}
	get.Flags().StringVarP(&out, "output", "o", "", "output file, or - for stdout (default: the version's file name)")
	get.Flags().BoolVar(&noCache, "no-cache", false, "do not read or fill the local verified cache")
	get.Flags().DurationVar(&stall, "stall-timeout", defaultStallTimeout, "abort an attempt when no bytes arrive for this long (0 disables); it is retried, resuming from <file>.part")
	art.AddCommand(get)

	var previewTTL time.Duration
	preview := &cobra.Command{
		Use:   "preview <version_id|artifact_id|title> [--ttl 5m]",
		Short: "Mint a short-lived, one-time isolated preview link for a version",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			vid, err := ops.ResolveVersionID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			pu, err := ops.API.MintPreviewURL(cmd.Context(), c.MintPreviewRequest{VersionID: vid, TTLSeconds: int(previewTTL / time.Second)})
			if err != nil {
				return err
			}
			return a.emit(pu, func(w io.Writer) {
				fmt.Fprintln(w, pu.URL)
				fmt.Fprintf(w, "  one use, expires %s (sandboxed; no management access)\n", stamp(pu.ExpiresAt))
			})
		}),
	}
	preview.Flags().DurationVar(&previewTTL, "ttl", 0, "link lifetime (default: hub default, 5m; max 1h)")
	art.AddCommand(preview)
	root.AddCommand(art)

	var cp CommentParams
	comment := &cobra.Command{
		Use:   `comment <version_or_revision_id> "text" [--anchor line:12-18]`,
		Short: "Comment on an exact artifact version or document revision",
		Args:  cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := cp
			p.Target, p.OperationID = args[0], a.opID
			if p.Text, err = a.readArg(args[1]); err != nil {
				return err
			}
			cm, err := ops.Comment(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(cm, func(w io.Writer) { fmt.Fprintf(w, "comment %s on %s %s\n", cm.CommentID, cm.Target.Kind, cm.Target.ID) })
		}),
	}
	comment.Flags().StringVar(&cp.Anchor, "anchor", "", "anchor: line:N-M, quote:TEXT, page:N, region:...")
	comment.Flags().StringVar(&cp.ReplyTo, "reply-to", "", "comment this replies to")
	comment.Flags().StringSliceVar(&cp.Mentions, "mention", nil, "session ID or owner to notify")
	root.AddCommand(comment)

	var cf CommentFilter
	commentsLs := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		f := cf
		if len(args) == 1 {
			f.Target = args[0]
		}
		page, err := ops.ListComments(cmd.Context(), f)
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) { printComments(w, page.Items) })
	}
	comments := &cobra.Command{
		Use:   "comments [target] [--unresolved]",
		Short: "List review comments for a version, revision, artifact or document",
		Args:  cobra.MaximumNArgs(1),
		RunE:  a.run(commentsLs),
	}
	comments.PersistentFlags().StringVar(&cf.Project, "project", "", "all comments in a project")
	comments.PersistentFlags().BoolVar(&cf.Unresolved, "unresolved", false, "only unresolved comments")
	comments.AddCommand(&cobra.Command{Use: "ls [target]", Short: "List comments", Args: cobra.MaximumNArgs(1), RunE: a.run(commentsLs)})
	var reopen bool
	resolve := &cobra.Command{
		Use:   "resolve <comment_id>",
		Short: "Resolve a comment (or --reopen)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			cm, err := ops.API.ResolveComment(cmd.Context(), c.ResolveCommentRequest{Mutation: mut(a.opID), CommentID: args[0], Resolved: !reopen})
			if err != nil {
				return err
			}
			return a.emit(cm, func(w io.Writer) { fmt.Fprintf(w, "comment %s resolved=%v\n", cm.CommentID, cm.Resolved) })
		}),
	}
	resolve.Flags().BoolVar(&reopen, "reopen", false, "mark unresolved again")
	comments.AddCommand(resolve)
	root.AddCommand(comments)
}

func printComments(w io.Writer, cs []c.ReviewComment) {
	for _, cm := range cs {
		st := ""
		if cm.Resolved {
			st = " [resolved]"
		}
		fmt.Fprintf(w, "%s on %s %s by %s %s%s\n", cm.CommentID, cm.Target.Kind, cm.Target.ID, principalText(cm.Author), ago(cm.CreatedAt), st)
		if cm.Anchor != nil {
			fmt.Fprintf(w, "  at %s %s\n", cm.Anchor.Kind, cm.Anchor.Value)
		}
		if cm.InReplyTo != "" {
			fmt.Fprintf(w, "  reply to %s\n", cm.InReplyTo)
		}
		for _, l := range strings.Split(strings.TrimRight(cm.Body, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
}

func safeFilename(name, fallback string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = unsafeName.ReplaceAllString(base, "_")
	base = strings.TrimLeft(base, ".")
	if base == "" || base == "_" {
		return fallback
	}
	return base
}

// artifactGet downloads exact published bytes. Bytes are verified against
// the version digest while writing; a file is only put in place (renamed)
// after verification succeeds. Progress goes to stderr; an attempt with no
// bytes for stall is aborted and resumed (artifact_download.go).
func (a *App) artifactGet(ctx context.Context, ops *Ops, versionID, out string, noCache bool, stall time.Duration) error {
	var ch *cache.Cache
	if !noCache {
		if cc, err := cache.Open(filepath.Join(DefaultStateDir(a.Getenv), "cache"), 512<<20); err == nil {
			ch = cc
		} else {
			a.warnf("pyre: artifact cache unavailable: %v", err)
		}
	}
	ver, err := ops.API.GetVersion(ctx, c.GetRequest{ID: versionID})
	if err != nil {
		return a.artifactGetCached(ch, versionID, out, err)
	}
	if out == "" {
		out = safeFilename(ver.Filename, ver.VersionID)
	}
	if out == "-" {
		if err := a.downloadToWriter(ctx, ops.API, ver, a.Stdout, stall); err != nil {
			return fmt.Errorf("%w (bytes already written to stdout are NOT the verified published version)", err)
		}
		return nil
	}
	if err := a.downloadToFile(ctx, ops.API, ver, out, stall); err != nil {
		if ch != nil && c.IsCode(err, c.ErrUnavailable) {
			return a.artifactGetCached(ch, versionID, out, err)
		}
		return err
	}
	if ch != nil {
		if f, err := os.Open(out); err == nil {
			if err := ch.PutVersion(ver, f); err != nil {
				a.warnf("pyre: could not cache %s: %v", ver.VersionID, err)
			}
			f.Close()
		}
	}
	return a.artifactWrote(ver, out)
}

// artifactGetCached serves a version from the verified local cache when
// the hub is unavailable; any other hub error is returned as is.
func (a *App) artifactGetCached(ch *cache.Cache, versionID, out string, hubErr error) error {
	if ch == nil || !c.IsCode(hubErr, c.ErrUnavailable) {
		return hubErr
	}
	ver, rc, err := ch.GetVersion(versionID)
	if err != nil {
		return hubErr
	}
	defer rc.Close()
	a.warnf("pyre: hub unavailable (%v); serving verified bytes from the local cache", hubErr)
	if out == "" {
		out = safeFilename(ver.Filename, ver.VersionID)
	}
	h := sha256.New()
	var n int64
	if out == "-" {
		n, err = io.Copy(io.MultiWriter(a.Stdout, h), rc)
		if err == nil {
			err = checkDigest(ver, n, h.Sum(nil))
		}
		if err != nil {
			return fmt.Errorf("%w (bytes already written to stdout are NOT the verified published version)", err)
		}
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".pyre-get-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err = io.Copy(io.MultiWriter(tmp, h), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = checkDigest(ver, n, h.Sum(nil))
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	return a.artifactWrote(ver, out)
}

func (a *App) artifactWrote(ver c.ArtifactVersion, out string) error {
	a.warnf("wrote %s (%d bytes, %s verified)", out, ver.Size, ver.Digest)
	return a.emit(ver, func(w io.Writer) {
		fmt.Fprintf(w, "%s\n", out)
	})
}

func checkDigest(v c.ArtifactVersion, n int64, sum []byte) error {
	got := "sha256:" + hex.EncodeToString(sum)
	if n != v.Size || !strings.EqualFold(got, string(v.Digest)) {
		return c.Errorf(c.ErrConflict, "downloaded bytes (%d, %s) do not match version %s (%d, %s)", n, got, v.VersionID, v.Size, v.Digest)
	}
	return nil
}
