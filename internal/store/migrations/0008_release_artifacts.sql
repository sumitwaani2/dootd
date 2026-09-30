-- Releases are GitHub release tarballs now (docs/architecture.md §7): the
-- id is the tag, subject the release name, sha256 the tarball's checksum.
-- git_sha, branch, subdir and zig_version stay empty for new releases.
ALTER TABLE releases ADD COLUMN sha256 TEXT NOT NULL DEFAULT '';
