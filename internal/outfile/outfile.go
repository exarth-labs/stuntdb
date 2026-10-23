// Package outfile commits output files atomically. An output is written
// under a temporary name in the folder it will end up in, and renamed to its
// final name only once it is complete, so that a file with the final name is
// always complete, even after a crash or a power cut.
//
// Errors never contain paths: the caller knows the path, and decides whether
// it is safe to show.
package outfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// retries is how often Commit tries a rename that failed for a reason that
// usually passes, such as an antivirus scanner holding the new file open on
// Windows. The waits add up to about two seconds.
const retries = 10

// Commit makes the finished file at partial durable, then renames it to
// final, replacing final if it exists. Both must be in the same folder.
func Commit(ctx context.Context, partial, final string) error {
	if err := prepare(partial, final); err != nil {
		return err
	}
	if err := retry(ctx, func() error { return os.Rename(partial, final) }); err != nil {
		return fmt.Errorf("outfile: rename the finished file into place: %w", pathless(err))
	}
	return finish(final)
}

// CommitNew is Commit for a file that must not replace another: if final
// exists, it fails with an error that wraps fs.ErrExist, even when another
// process creates final at the same moment. It links the partial file to its
// final name, because a link, unlike a rename, never replaces a file, and
// then removes the partial name.
func CommitNew(ctx context.Context, partial, final string) error {
	if err := prepare(partial, final); err != nil {
		return err
	}
	if err := retry(ctx, func() error { return os.Link(partial, final) }); err != nil {
		return fmt.Errorf("outfile: link the finished file into place: %w", pathless(err))
	}
	if err := retry(ctx, func() error { return os.Remove(partial) }); err != nil {
		return fmt.Errorf("outfile: the file is in place, but its partial name cannot be removed: %w", pathless(err))
	}
	return finish(final)
}

// prepare checks the two names and makes the finished file durable.
func prepare(partial, final string) error {
	if filepath.Dir(partial) != filepath.Dir(final) {
		return errors.New("outfile: a partial file must be in the same folder as its final name")
	}
	if err := syncFile(partial); err != nil {
		return fmt.Errorf("outfile: flush the finished file to disk: %w", pathless(err))
	}
	return nil
}

// finish makes the new name durable.
func finish(final string) error {
	if err := syncDir(filepath.Dir(final)); err != nil {
		return fmt.Errorf("outfile: flush the folder to disk: %w", pathless(err))
	}
	return nil
}

// retry runs op until it succeeds, fails for a reason that does not pass,
// has run retries times, or ctx is canceled.
func retry(ctx context.Context, op func() error) error {
	wait := 20 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt == retries || !transient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, 250*time.Millisecond)
	}
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // the caller's own output file
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable. Windows makes renames durable
// itself, and cannot open a folder for syncing.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir) //nolint:gosec // dir is the caller's output folder
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// pathless returns the cause of a file-system error without the paths that
// os adds to it.
func pathless(err error) error {
	if e, ok := errors.AsType[*fs.PathError](err); ok {
		return e.Err
	}
	if e, ok := errors.AsType[*os.LinkError](err); ok {
		return e.Err
	}
	return err
}
