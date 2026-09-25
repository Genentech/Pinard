# Vendored makeself (patched)

Vendored from [makeself](https://github.com/megastep/makeself) release-2.5.0,
with a single patch needed to package Pinard's large bundle (~82k files).

## The patch

Stock makeself builds the inner tarball with:

```sh
find . ... -print | LC_ALL=C sort | sed 's/./\\&/g' | xargs $TAR ... -$TAR_ARGS "$tmparch"
```

With ~100k files the path list exceeds `ARG_MAX`, so `xargs` splits it into
multiple `tar` invocations. Because `$TAR_ARGS` uses create/replace mode, each
batch **truncates** the archive — only the last batch's files survive, and the
build fails ("failed to create temporary archive").

Patched to feed all paths to a single `tar` via NUL-delimited stdin:

```sh
find . ... -print0 | LC_ALL=C sort -z | $TAR ... --null -T - -$TAR_ARGS "$tmparch"
```

(in `makeself.sh`, the archive-creation block around line 632.)

`dist/build.sh` also passes `--tar-format gnu` because deep `node_modules` paths
exceed the default ustar 100/255-char path limit.
