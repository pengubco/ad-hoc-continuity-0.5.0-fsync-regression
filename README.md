# Per-File fsync in Sparse-Aware copyFile: A 17x Performance Regression

## Summary

Commit [`a424ba1`](https://github.com/containerd/continuity/commit/a424ba1d1c4508ac5db9d48a0cd0b70aa5d54fd6) ("Linux: Make copyFile sparse aware") introduced sparse file support to `continuity`'s `fs.CopyFile` on Linux. The change is valuable — it preserves sparse file holes during copy, avoiding unnecessary disk allocation. However, it also added a per-file `fsync` (`tgt.Sync()`) that causes a severe performance regression when copying directories with many files — the dominant workload for container image extraction and volume copy-up.

## The Change

Before `a424ba1`, `copyFile` on Linux was a simple `io.Copy`:

```go
func openAndCopyFile(target, source string) error {
    src, err := os.Open(source)
    defer src.Close()
    tgt, err := os.Create(target)
    defer tgt.Close()
    _, err = io.Copy(tgt, src)
    return err
}
```

After the change, the new implementation uses `SEEK_DATA`/`SEEK_HOLE` to skip holes, `copy_file_range` for zero-copy data transfer, and calls `tgt.Sync()` before returning:

```go
func copyFile(target, source string) error {
    // ... open, truncate, SEEK_DATA/SEEK_HOLE loop with copy_file_range ...

    if err := tgt.Sync(); err != nil {
        return fmt.Errorf("failed to sync target %s: %w", target, err)
    }
    return nil
}
```

## The Problem

`fsync` forces the kernel to flush all dirty pages for the file to disk and wait for the I/O to complete. When you call it once per file across thousands of files, you're issuing thousands of individual disk flush operations. Each one waits for the storage device to confirm the write — this serializes what would otherwise be batched writeback.

The old `io.Copy` path had no `fsync`. Data stayed in the kernel's page cache after `write()` returned, and the kernel's writeback daemon flushed it to disk in batches on its own schedule. This is perfectly acceptable for container workloads — if the machine crashes mid-operation, you discard the partial result and redo it.

## Benchmark

Test setup: 50,000 regular files, each 10 KiB, filled with random data. Copied on the same ext4 filesystem. Page cache dropped between runs (`echo 3 > /proc/sys/vm/drop_caches`).

| Copy method | Sync mode | Time | Relative |
|---|---|---|---|
| Naive (`io.Copy`, no fsync) | none | **4.35s** | 1.0x |
| Sparse-aware (`SEEK_DATA` + `copy_file_range`) | none | 4.84s | 1.1x |
| Sparse-aware | single sync at end of dir | 4.88s | 1.1x |
| Sparse-aware | **per-file fsync** | **1m 16s** | **17.4x** |

### Observations

1. **The fsync is the bottleneck, not the sparse logic.** Without fsync, the sparse-aware copy is only ~10% slower than naive `io.Copy` (due to the extra `SEEK_DATA` + `SEEK_HOLE` syscalls per file).

2. **Per-file fsync is catastrophic for many-files workloads.** 50K fsyncs take over a minute vs 4.4 seconds without.

3. **A single `sync()` at the end of the directory copy achieves the same durability** as per-file fsync (all data reaches disk before the operation returns) but amortizes the cost across all files.

## Impact on Containers

`continuity/fs.CopyDir` (which calls `fs.CopyFile` → `copyFile` for each regular file) is used in several critical paths:

### Volume Copy-Up (Docker VOLUME Directive)

When a Dockerfile declares `VOLUME /path`, and the image has existing content at that path, the container runtime must copy that content into the volume mount at container start. In containerd's CRI plugin, this is the `volumeCopyUp` path (`pkg/cri/server/container_create_linux.go`) which calls `fs.CopyDir`. If the image has thousands of files in the volume path (e.g., a `/var/lib` with application data, a `/usr/share` with assets), the per-file fsync adds significant startup latency.

This was already reported as a performance issue in [containerd/containerd#8639](https://github.com/containerd/containerd/issues/8639) — "Image-defined volumes with lots of files cause high I/O load at container start."

### Other CopyDir Users

- `nerdctl cp` — copying files into/out of containers
- Snapshot operations that replicate directory trees
- Any tool using `continuity/fs` for filesystem operations

## Why Per-File fsync is Unnecessary Here

The old code was never "safe" from a crash perspective — it just didn't need to be. For these workloads:

- **Volume copy-up**: If the machine crashes during copy-up, the container never started successfully. On restart, the runtime redoes the copy-up from the immutable image layer.
- **Image extraction**: If extraction is interrupted, the partial snapshot is discarded and re-extracted.
- **File copy operations**: The caller controls whether durability matters.

None of these require per-file durability guarantees. The data source (image layer) is always available for replay.

## Suggested Fix

Remove the per-file `tgt.Sync()` from `copyFile`. If some callers genuinely need durability, options include:

1. A single `sync()` at the end of `CopyDir` (same durability, amortized cost — adds <1% overhead in benchmarks).
2. A `CopyDirOpt` to opt in to per-file fsync for callers that need it.

The sparse-aware copy logic (`SEEK_DATA`/`SEEK_HOLE` + `copy_file_range`) should remain — it's the right approach for sparse files and only adds ~10% overhead for non-sparse files.

## Reproducing

```bash
git clone https://github.com/pengubco/ad-hoc-continuity-0.5.0-fsync-regression
cd ad-hoc-continuity-0.5.0-fsync-regression

# Create test data (50K files × 10KiB)
go run ./create-many/ -n 50000 -size 10240 ./_bench/source

# Run benchmark (requires root for drop_caches)
go build -o copy-bench-bin ./copy-bench/
sudo ./copy-bench-bin -sync per-file ./_bench/source   # 1m16s — current behavior
sudo ./copy-bench-bin -sync none ./_bench/source       # 4.8s  — without fsync
sudo ./copy-bench-bin -sync dir ./_bench/source        # 4.9s  — single sync at end
```
