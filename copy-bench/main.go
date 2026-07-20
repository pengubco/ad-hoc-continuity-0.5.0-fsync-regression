//go:build linux

// copy-bench benchmarks copying a directory using two strategies:
//  1. Naive: io.Copy (reads zeros from holes, writes them out)
//  2. Sparse-aware: SEEK_DATA/SEEK_HOLE + copy_file_range (skips holes)
//
// Usage: copy-bench <source-dir>
//
// It copies the source directory twice (to temp dirs), timing each approach,
// then prints time taken and disk usage for each.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var syncMode string

func main() {
	flag.StringVar(&syncMode, "sync", "per-file", "sync mode for sparse copy: none, per-file, dir")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: copy-bench [-sync none|per-file|dir] <source-dir>\n")
		os.Exit(1)
	}
	srcDir := flag.Arg(0)

	fi, err := os.Stat(srcDir)
	if err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "%s is not a directory\n", srcDir)
		os.Exit(1)
	}

	// Use a directory next to the source so we stay on the same filesystem.
	// This avoids EXDEV errors with copy_file_range across mount points.
	parentDir := filepath.Dir(srcDir)
	outDir := filepath.Join(parentDir, "_copy_bench_output")
	os.RemoveAll(outDir)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir output: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(outDir)

	naiveDir := filepath.Join(outDir, "naive")
	sparseDir := filepath.Join(outDir, "sparse")

	// --- Sparse-aware copy (cold cache) ---
	dropCaches()
	fmt.Printf("Sync mode: %s\n\n", syncMode)
	start := time.Now()
	if err := copyDirSparse(sparseDir, srcDir); err != nil {
		fmt.Fprintf(os.Stderr, "sparse copy: %v\n", err)
		os.Exit(1)
	}
	if syncMode == "dir" {
		syncDir(sparseDir)
	}
	sparseTime := time.Since(start)
	sparseFiles, sparseDisk := countDir(sparseDir)

	// Clean up sparse output
	os.RemoveAll(sparseDir)

	// --- Naive copy (cold cache) ---
	dropCaches()
	start = time.Now()
	if err := copyDirNaive(naiveDir, srcDir); err != nil {
		fmt.Fprintf(os.Stderr, "naive copy: %v\n", err)
		os.Exit(1)
	}
	naiveTime := time.Since(start)
	naiveFiles, naiveDisk := countDir(naiveDir)

	// --- Results ---
	fmt.Printf("Naive copy (io.Copy):\n")
	fmt.Printf("  Time:       %v\n", naiveTime)
	fmt.Printf("  Files:      %d\n", naiveFiles)
	fmt.Printf("  Disk usage: %s\n\n", humanSize(naiveDisk))

	fmt.Printf("Sparse-aware copy (SEEK_DATA/SEEK_HOLE + copy_file_range):\n")
	fmt.Printf("  Time:       %v\n", sparseTime)
	fmt.Printf("  Files:      %d\n", sparseFiles)
	fmt.Printf("  Disk usage: %s\n\n", humanSize(sparseDisk))

	// --- Summary ---
	fmt.Printf("Disk savings: %s (%.1f%% less)\n",
		humanSize(naiveDisk-sparseDisk),
		float64(naiveDisk-sparseDisk)/float64(naiveDisk)*100)
	if sparseTime > 0 {
		fmt.Printf("Time ratio:   naive=%.2fx of sparse\n", float64(naiveTime)/float64(sparseTime))
	}
}

// --- Naive copy: walks the tree and uses io.Copy for regular files ---

func copyDirNaive(dst, src string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return naiveCopyFile(target, path)
	})
}

func naiveCopyFile(dst, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return err
}

// --- Sparse-aware copy: walks the tree and uses SEEK_DATA/SEEK_HOLE ---

func copyDirSparse(dst, src string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return sparseAwareCopyFile(target, path)
	})
}

func sparseAwareCopyFile(dst, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	fi, err := s.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	// Truncate to set logical size; unwritten regions stay as holes.
	if err := d.Truncate(size); err != nil {
		return err
	}

	srcFd := int(s.Fd())
	dstFd := int(d.Fd())

	// Check if SEEK_DATA is supported
	if _, err := unix.Seek(srcFd, 0, unix.SEEK_DATA); err != nil {
		if err == syscall.ENXIO {
			// Entirely sparse — truncated target is already correct
			return nil
		}
		// SEEK_DATA not supported, fall back to io.Copy
		if err == syscall.EOPNOTSUPP || err == syscall.ENOTSUP || err == syscall.EINVAL {
			s.Close()
			d.Close()
			return naiveCopyFile(dst, src)
		}
		return err
	}

	var offset int64
	for offset < size {
		dataStart, err := unix.Seek(srcFd, offset, unix.SEEK_DATA)
		if err != nil {
			if err == syscall.ENXIO {
				break
			}
			return err
		}

		holeStart, err := unix.Seek(srcFd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			if err == syscall.ENXIO {
				holeStart = size
			} else {
				return err
			}
		}

		// Copy data region [dataStart, holeStart)
		srcOff := dataStart
		dstOff := dataStart
		remain := holeStart - dataStart
		for remain > 0 {
			chunk := remain
			if chunk > 1<<30 {
				chunk = 1 << 30
			}
			n, err := unix.CopyFileRange(srcFd, &srcOff, dstFd, &dstOff, int(chunk), 0)
			if err != nil {
				return fmt.Errorf("copy_file_range: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("copy_file_range returned 0")
			}
			remain -= int64(n)
		}

		offset = holeStart
	}

	if syncMode == "per-file" {
		if err := d.Sync(); err != nil {
			return fmt.Errorf("failed to sync target %s: %w", dst, err)
		}
	}

	return nil
}

// --- Helpers ---

func countDir(dir string) (files int, diskBytes int64) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			files++
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				diskBytes += st.Blocks * 512
			}
		}
		return nil
	})
	return
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// syncDir does a single sync on the entire directory tree (equivalent to calling sync()).
func syncDir(dir string) {
	unix.Sync()
}

// dropCaches attempts to drop the kernel page cache so both copy methods
// start with cold cache. Requires root; silently skipped if not root.
func dropCaches() {
	// sync first to flush dirty pages
	unix.Sync()
	// echo 3 > /proc/sys/vm/drop_caches (free page cache, dentries, inodes)
	err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [warning] could not drop caches (run as root for fair comparison): %v\n", err)
	}
}
