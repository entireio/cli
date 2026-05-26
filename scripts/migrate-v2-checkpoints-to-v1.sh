#!/usr/bin/env bash
set -euo pipefail

#
# migrate-v2-checkpoints-to-v1.sh - Migrate legacy v2 checkpoints to v1.
#
# USAGE:
#   ./scripts/migrate-v2-checkpoints-to-v1.sh [OPTIONS] [SINCE_COMMIT]
#
# OPTIONS:
#   -h, --help            Show this help message
#   --list                Print checkpoint IDs and associated commit IDs only
#   --dry-run             Print every v2 folder/file that would be migrated
#   --apply               Write local refs/heads/entire/checkpoints/v1 migration commits
#   --repo <path>         Local repository path to inspect
#   --since <commit>      Commit before the checkpoints to inspect
#   --head <commit>       Limit scan to one history tip (default: all branches/remotes)
#
# DESCRIPTION:
#   Standalone helper for converting legacy checkpoints v2 data back to the v1
#   checkpoint format. The script finds commits newer than SINCE_COMMIT on local
#   branches/remotes (or on --head, when supplied), extracts Entire-Checkpoint
#   trailers, and locates the v2 /full folders/files that contain raw transcripts:
#
#     refs/entire/checkpoints/v2/full/*:<checkpoint-path>/<session>/raw_transcript*
#
#   The default mode prints a migration plan without writing refs. --dry-run
#   prints every source folder/file. --apply writes one local migration commit
#   per checkpoint to refs/heads/entire/checkpoints/v1.
#
#   If --repo or SINCE_COMMIT is omitted, the script prompts for it.
#

V1_REF="refs/heads/entire/checkpoints/v1"
V2_MAIN_REF="refs/entire/checkpoints/v2/main"
V2_FULL_REF_PREFIX="refs/entire/checkpoints/v2/full"
TRAILER_KEY="Entire-Checkpoint"

since_commit=""
head_commitish=""
repo_path=""
dry_run=false
apply=false
list_mode=false
tmp_dir=""
plan_entries_file=""
checkpoint_commits_file=""
checkpoint_ids_file=""
checkpoint_paths_file=""
full_artifacts_file=""
raw_sessions_file=""
raw_checkpoint_ids_file=""
main_metadata_file=""

show_help() {
	sed -n '5,/^$/p' "$0" | sed -E 's/^# ?//'
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

warn() {
	printf 'warning: %s\n' "$*" >&2
}

cleanup() {
	if [[ -n "$tmp_dir" && -d "$tmp_dir" ]]; then
		rm -rf "$tmp_dir"
	fi
}
trap cleanup EXIT

checkpoint_to_path() {
	local checkpoint_id="$1"
	printf '%s/%s' "${checkpoint_id:0:2}" "${checkpoint_id:2}"
}

list_full_refs() {
	git for-each-ref --format='%(refname)' "$V2_FULL_REF_PREFIX" |
		sort |
		awk -v current="${V2_FULL_REF_PREFIX}/current" '
			$0 == current { current_ref = $0; next }
			{ refs[++n] = $0 }
			END {
				if (current_ref != "") {
					print current_ref
				}
				for (i = n; i >= 1; i--) {
					print refs[i]
				}
			}
		'
}

find_bulk_migration_ancestor() {
	local head_hash="$1"

	git log --first-parent --format='%H%x09%s' "$head_hash" |
		awk -F '\t' '$2 == "Migrate checkpoints v2 to v1" { print $1; exit }'
}

write_checkpoint_commit_index_between() {
	local since="$1"
	local head="$2"
	local output_file="$3"

	git log --format='__ENTIRE_COMMIT__%H%n%B' "${since}..${head}" |
		awk -v key="$TRAILER_KEY" '
			/^__ENTIRE_COMMIT__/ {
				commit = substr($0, length("__ENTIRE_COMMIT__") + 1)
				next
			}
			{
				line = $0
				pattern = "^[[:space:]]*" key ":[[:space:]]*([0-9a-f]{12})[[:space:]]*$"
				if (line ~ pattern) {
					sub("^[[:space:]]*" key ":[[:space:]]*", "", line)
					sub("[[:space:]]*$", "", line)
					if (commit != "" && !seen[line SUBSEP commit]++) {
						print line "\t" commit
					}
				}
			}
		' > "$output_file"
}

write_checkpoint_commit_index_from_all_refs() {
	local since="$1"
	local output_file="$2"
	local refs_file="$tmp_dir/refs_containing_since"

	: > "$refs_file"
	git for-each-ref --contains "$since" --format='%(refname)' refs/heads refs/remotes > "$refs_file"
	if git merge-base --is-ancestor "$since" HEAD 2>/dev/null; then
		printf 'HEAD\n' >> "$refs_file"
	fi
	sort -u "$refs_file" -o "$refs_file"

	if [[ ! -s "$refs_file" ]]; then
		: > "$output_file"
		return
	fi

	xargs git log --format='__ENTIRE_COMMIT__%H%n%B' "$since".. < "$refs_file" |
		awk -v key="$TRAILER_KEY" '
			/^__ENTIRE_COMMIT__/ {
				commit = substr($0, length("__ENTIRE_COMMIT__") + 1)
				next
			}
			{
				line = $0
				pattern = "^[[:space:]]*" key ":[[:space:]]*([0-9a-f]{12})[[:space:]]*$"
				if (line ~ pattern) {
					sub("^[[:space:]]*" key ":[[:space:]]*", "", line)
					sub("[[:space:]]*$", "", line)
					if (commit != "" && !seen[line SUBSEP commit]++) {
						print line "\t" commit
					}
				}
			}
		' > "$output_file"
}

write_unique_mktree_input() {
	local entries_file="$1"
	awk -F '\t' 'NF >= 2 && !seen[$2]++ { print }' "$entries_file" |
		sort -k2
}

build_v1_tree_from_entries() {
	local base_tree_hash="$1"
	local entries_file="$2"
	local combined index_file
	combined="$tmp_dir/combined_index_info"
	index_file="$tmp_dir/migration.index"

	rm -f "$index_file"
	cat "$entries_file" > "$combined"
	if [[ -n "$base_tree_hash" ]]; then
		git ls-tree -r "$base_tree_hash" >> "$combined"
	fi

	write_unique_mktree_input "$combined" |
		GIT_INDEX_FILE="$index_file" git update-index --index-info
	GIT_INDEX_FILE="$index_file" git write-tree
}

create_v1_checkpoint_migration_commit() {
	local checkpoint_id="$1"
	local tree_hash="$2"
	local parent_hash="$3"
	local commit_date="$4"
	local commit_hash

	if [[ -n "$parent_hash" ]]; then
		if [[ -n "$commit_date" ]]; then
			commit_hash=$(printf 'Checkpoint: %s\n\nMigrated from checkpoints v2.\nSource refs: %s and %s/*\n' "$checkpoint_id" "$V2_MAIN_REF" "$V2_FULL_REF_PREFIX" |
				GIT_AUTHOR_DATE="$commit_date" GIT_COMMITTER_DATE="$commit_date" git commit-tree "$tree_hash" -p "$parent_hash")
		else
			commit_hash=$(printf 'Checkpoint: %s\n\nMigrated from checkpoints v2.\nSource refs: %s and %s/*\n' "$checkpoint_id" "$V2_MAIN_REF" "$V2_FULL_REF_PREFIX" |
				git commit-tree "$tree_hash" -p "$parent_hash")
		fi
	else
		if [[ -n "$commit_date" ]]; then
			commit_hash=$(printf 'Checkpoint: %s\n\nMigrated from checkpoints v2.\nSource refs: %s and %s/*\n' "$checkpoint_id" "$V2_MAIN_REF" "$V2_FULL_REF_PREFIX" |
				GIT_AUTHOR_DATE="$commit_date" GIT_COMMITTER_DATE="$commit_date" git commit-tree "$tree_hash")
		else
			commit_hash=$(printf 'Checkpoint: %s\n\nMigrated from checkpoints v2.\nSource refs: %s and %s/*\n' "$checkpoint_id" "$V2_MAIN_REF" "$V2_FULL_REF_PREFIX" |
				git commit-tree "$tree_hash")
		fi
	fi

	printf '%s\n' "$commit_hash"
}

write_checkpoint_id_files() {
	awk -F '\t' 'NF >= 2 && !seen[$1]++ { print $1 }' "$checkpoint_commits_file" > "$checkpoint_ids_file"
	awk 'NF { print $0 "\t" substr($0, 1, 2) "/" substr($0, 3) }' "$checkpoint_ids_file" > "$checkpoint_paths_file"
}

write_apply_checkpoint_ids() {
	local output_file="$1"

	awk '
		NR == FNR {
			raw[$1] = 1
			next
		}
		NF && ($1 in raw) {
			ids[++n] = $1
		}
		END {
			for (i = n; i >= 1; i--) {
				print ids[i]
			}
		}
	' "$raw_checkpoint_ids_file" "$checkpoint_ids_file" > "$output_file"
}

write_bulk_migration_entries() {
	local bulk_commit="$1"
	local parent_commit="$2"
	local output_entries_file="$3"
	local output_ids_file="$4"
	local changed_paths_file checkpoint_dirs_file

	changed_paths_file="$tmp_dir/bulk_changed_paths"
	checkpoint_dirs_file="$tmp_dir/bulk_checkpoint_dirs"

	if [[ -n "$parent_commit" ]]; then
		git diff --name-only "$parent_commit" "$bulk_commit" > "$changed_paths_file"
	else
		git ls-tree -r --name-only "$bulk_commit" > "$changed_paths_file"
	fi

	awk -F '/' '
		NF >= 3 && $1 ~ /^[0-9a-f][0-9a-f]$/ && $2 ~ /^[0-9a-f]{10}$/ {
			dir = $1 "/" $2
			if (!seen[dir]++) {
				print dir
			}
		}
	' "$changed_paths_file" > "$checkpoint_dirs_file"

	awk -F '/' 'NF >= 2 { print $1 $2 }' "$checkpoint_dirs_file" > "$output_ids_file"

	git ls-tree -r "$bulk_commit" |
		awk -F '\t' -v checkpoint_dirs_file="$checkpoint_dirs_file" '
			BEGIN {
				while ((getline dir < checkpoint_dirs_file) > 0) {
					prefixes[dir "/"] = 1
				}
				close(checkpoint_dirs_file)
			}
			NF >= 2 {
				for (prefix in prefixes) {
					if (index($2, prefix) == 1) {
						print
						next
					}
				}
			}
		' > "$output_entries_file"
}

write_checkpoint_entries() {
	local checkpoint_id="$1"
	local output_file="$2"
	local source_entries_file="$3"
	local checkpoint_path

	checkpoint_path=$(checkpoint_to_path "$checkpoint_id")
	awk -F '\t' -v prefix="${checkpoint_path}/" 'NF >= 2 && index($2, prefix) == 1 { print }' "$source_entries_file" > "$output_file"
}

write_checkpoint_commit_dates() {
	local source_entries_file="$1"
	local output_file="$2"

	command -v python3 >/dev/null 2>&1 || die "python3 is required for --apply metadata date extraction"

	python3 - "$source_entries_file" "$output_file" <<'PY'
import json
import re
import subprocess
import sys

source_entries_file, output_file = sys.argv[1:]
metadata_re = re.compile(r"^([0-9a-f]{2})/([0-9a-f]{10})/[0-9]+/metadata\.json$")

records = []
with open(source_entries_file, "r", encoding="utf-8") as f:
    for line in f:
        line = line.rstrip("\n")
        if not line or "\t" not in line:
            continue
        object_info, path = line.split("\t", 1)
        match = metadata_re.match(path)
        if not match:
            continue
        object_parts = object_info.split()
        if len(object_parts) != 3 or object_parts[1] != "blob":
            continue
        checkpoint_id = match.group(1) + match.group(2)
        records.append((checkpoint_id, object_parts[2]))

if not records:
    open(output_file, "w", encoding="utf-8").close()
    sys.exit(0)

batch_input = "".join(blob + "\n" for _, blob in records).encode("ascii")
batch = subprocess.run(
    ["git", "cat-file", "--batch"],
    input=batch_input,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    check=False,
)
if batch.returncode != 0:
    sys.stderr.write(batch.stderr.decode("utf-8", errors="replace"))
    sys.exit(batch.returncode)

dates = {}
out = batch.stdout
offset = 0
for checkpoint_id, blob in records:
    header_end = out.find(b"\n", offset)
    if header_end < 0:
        sys.stderr.write(f"missing git cat-file header for {blob}\n")
        sys.exit(1)
    header = out[offset:header_end].decode("ascii", errors="replace")
    offset = header_end + 1
    parts = header.split()
    if len(parts) < 3 or parts[1] != "blob":
        sys.stderr.write(f"unexpected git cat-file header for {blob}: {header}\n")
        sys.exit(1)
    size = int(parts[2])
    data = out[offset:offset + size]
    offset += size
    if offset >= len(out) or out[offset:offset + 1] != b"\n":
        sys.stderr.write(f"missing git cat-file record separator for {blob}\n")
        sys.exit(1)
    offset += 1

    metadata = json.loads(data.decode("utf-8"))
    created_at = metadata.get("created_at")
    if created_at and (checkpoint_id not in dates or created_at < dates[checkpoint_id]):
        dates[checkpoint_id] = created_at

with open(output_file, "w", encoding="utf-8") as f:
    for checkpoint_id in sorted(dates):
        f.write(f"{checkpoint_id}\t{dates[checkpoint_id]}\n")
PY
}

order_checkpoint_ids_by_date() {
	local checkpoint_ids_source_file="$1"
	local checkpoint_dates_file="$2"
	local output_file="$3"

	awk -F '\t' '
		NR == FNR {
			date[$1] = $2
			next
		}
		NF {
			order++
			sort_date = ($1 in date && date[$1] != "") ? date[$1] : sprintf("9999-12-31T23:59:59Z-%09d", order)
			print sort_date "\t" sprintf("%09d", order) "\t" $1
		}
	' "$checkpoint_dates_file" "$checkpoint_ids_source_file" |
		sort -t $'\t' -k1,1 -k2,2 |
		cut -f3 > "$output_file"
}

checkpoint_commit_date() {
	local checkpoint_id="$1"
	local checkpoint_dates_file="$2"

	awk -F '\t' -v checkpoint_id="$checkpoint_id" '$1 == checkpoint_id { print $2; exit }' "$checkpoint_dates_file"
}

write_full_artifact_index() {
	local full_ref
	: > "$full_artifacts_file"
	: > "$raw_sessions_file"
	: > "$raw_checkpoint_ids_file"

	while IFS= read -r full_ref; do
		[[ -n "$full_ref" ]] || continue
		git ls-tree -r "$full_ref" |
			awk -F '\t' \
				-v ref="$full_ref" \
				-v checkpoint_paths_file="$checkpoint_paths_file" \
				-v full_artifacts_file="$full_artifacts_file" \
				-v raw_sessions_file="$raw_sessions_file" \
				-v raw_checkpoint_ids_file="$raw_checkpoint_ids_file" \
				-v plan_entries_file="$plan_entries_file" '
				BEGIN {
					while ((getline line < checkpoint_paths_file) > 0) {
						split(line, fields, "\t")
						checkpoint_by_path[fields[2]] = fields[1]
					}
				}
				NF >= 2 {
					meta = $1
					path = $2
					n = split(path, parts, "/")
					if (n != 4) {
						next
					}
					checkpoint_path = parts[1] "/" parts[2]
					if (!(checkpoint_path in checkpoint_by_path)) {
						next
					}
					session_index = parts[3]
					artifact = parts[4]
					if (artifact == "raw_transcript") {
						target = checkpoint_path "/" session_index "/full.jsonl"
					} else if (artifact ~ /^raw_transcript\.[0-9][0-9][0-9]$/) {
						suffix = artifact
						sub(/^raw_transcript/, "", suffix)
						target = checkpoint_path "/" session_index "/full.jsonl" suffix
					} else if (artifact == "raw_transcript_hash.txt") {
						target = checkpoint_path "/" session_index "/content_hash.txt"
					} else {
						next
					}
					checkpoint_id = checkpoint_by_path[checkpoint_path]
					print checkpoint_id "\t" checkpoint_path "\t" session_index "\t" ref "\t" artifact "\t" path "\t" target "\t" meta >> full_artifacts_file
					print checkpoint_id "\t" checkpoint_path "\t" session_index >> raw_sessions_file
					print checkpoint_id >> raw_checkpoint_ids_file
					print meta "\t" target >> plan_entries_file
				}
			'
	done <<< "$full_refs"

	sort -u "$raw_sessions_file" -o "$raw_sessions_file"
	awk 'NF && !seen[$0]++' "$raw_checkpoint_ids_file" > "${raw_checkpoint_ids_file}.tmp"
	mv "${raw_checkpoint_ids_file}.tmp" "$raw_checkpoint_ids_file"
}

write_main_metadata_index() {
	: > "$main_metadata_file"
	if [[ "$main_ref_available" != "true" ]]; then
		return
	fi

	git ls-tree -r "$V2_MAIN_REF" |
		awk -F '\t' \
			-v checkpoint_paths_file="$checkpoint_paths_file" \
			-v raw_sessions_file="$raw_sessions_file" \
			-v main_metadata_file="$main_metadata_file" \
			-v plan_entries_file="$plan_entries_file" '
			BEGIN {
				while ((getline line < checkpoint_paths_file) > 0) {
					split(line, fields, "\t")
					checkpoint_by_path[fields[2]] = fields[1]
				}
				while ((getline line < raw_sessions_file) > 0) {
					split(line, fields, "\t")
					session_wanted[fields[2] "/" fields[3]] = 1
					checkpoint_has_raw[fields[2]] = 1
				}
			}
			NF >= 2 {
				meta = $1
				path = $2
				n = split(path, parts, "/")
				checkpoint_path = parts[1] "/" parts[2]
				if (!(checkpoint_path in checkpoint_by_path)) {
					next
				}
				checkpoint_id = checkpoint_by_path[checkpoint_path]
				if (n == 3 && parts[3] == "metadata.json") {
					if (!(checkpoint_path in checkpoint_has_raw)) {
						next
					}
					print checkpoint_id "\t" checkpoint_path "\t-\tcheckpoint_metadata\t" path "\t" meta >> main_metadata_file
					next
				}
				if (n == 4 && (parts[4] == "metadata.json" || parts[4] == "prompt.txt")) {
					session_key = checkpoint_path "/" parts[3]
					if (!(session_key in session_wanted)) {
						next
					}
					kind = parts[4] == "metadata.json" ? "session_metadata" : "prompt"
					print checkpoint_id "\t" checkpoint_path "\t" parts[3] "\t" kind "\t" path "\t" meta >> main_metadata_file
					print meta "\t" path >> plan_entries_file
				}
			}
		'
}

append_checkpoint_metadata_plan_entries() {
	if ! awk -F '\t' '$4 == "checkpoint_metadata" { found = 1; exit } END { exit found ? 0 : 1 }' "$main_metadata_file"; then
		return
	fi

	if [[ "$apply" == "true" ]]; then
		rewrite_checkpoint_metadata_plan_entries
		return
	fi

	awk -F '\t' '$4 == "checkpoint_metadata" { print $6 "\t" $5 }' "$main_metadata_file" >> "$plan_entries_file"
}

rewrite_checkpoint_metadata_plan_entries() {
	command -v python3 >/dev/null 2>&1 || die "python3 is required for --apply metadata rewriting"

	local rewrite_dir rewrite_manifest rewrite_paths rewrite_hashes
	rewrite_dir="$tmp_dir/rewritten-checkpoint-metadata"
	rewrite_manifest="$tmp_dir/rewritten-checkpoint-metadata.tsv"
	rewrite_paths="$tmp_dir/rewritten-checkpoint-metadata.paths"
	rewrite_hashes="$tmp_dir/rewritten-checkpoint-metadata.hashes"

	mkdir -p "$rewrite_dir"

	python3 - "$main_metadata_file" "$raw_sessions_file" "$rewrite_dir" "$rewrite_manifest" "$rewrite_paths" <<'PY'
import json
import os
import subprocess
import sys

main_metadata_file, raw_sessions_file, rewrite_dir, rewrite_manifest, rewrite_paths = sys.argv[1:]

sessions_by_checkpoint = {}
with open(raw_sessions_file, "r", encoding="utf-8") as f:
    for line in f:
        line = line.rstrip("\n")
        if not line:
            continue
        checkpoint_id, checkpoint_path, session_index = line.split("\t")
        del checkpoint_id
        sessions_by_checkpoint.setdefault(checkpoint_path, set()).add(session_index)

records = []
with open(main_metadata_file, "r", encoding="utf-8") as f:
    for line in f:
        line = line.rstrip("\n")
        if not line:
            continue
        fields = line.split("\t")
        if len(fields) < 6:
            continue
        checkpoint_id, checkpoint_path, session_index, kind, target_path, object_info = fields[:6]
        del checkpoint_id, session_index
        if kind == "checkpoint_metadata" and checkpoint_path in sessions_by_checkpoint:
            object_parts = object_info.split()
            if len(object_parts) != 3 or object_parts[1] != "blob":
                sys.stderr.write(f"unexpected ls-tree metadata for {target_path}: {object_info}\n")
                sys.exit(1)
            records.append((object_parts[2], target_path, checkpoint_path))

if not records:
    open(rewrite_manifest, "w", encoding="utf-8").close()
    open(rewrite_paths, "w", encoding="utf-8").close()
    sys.exit(0)

batch_input = "".join(blob + "\n" for blob, _, _ in records).encode("ascii")
batch = subprocess.run(
    ["git", "cat-file", "--batch"],
    input=batch_input,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    check=False,
)
if batch.returncode != 0:
    sys.stderr.write(batch.stderr.decode("utf-8", errors="replace"))
    sys.exit(batch.returncode)

out = batch.stdout
offset = 0

with open(rewrite_manifest, "w", encoding="utf-8") as manifest, open(rewrite_paths, "w", encoding="utf-8") as paths:
    for blob, target_path, checkpoint_path in records:
        header_end = out.find(b"\n", offset)
        if header_end < 0:
            sys.stderr.write(f"missing git cat-file header for {blob}\n")
            sys.exit(1)
        header = out[offset:header_end].decode("ascii", errors="replace")
        offset = header_end + 1
        parts = header.split()
        if len(parts) < 3 or parts[1] != "blob":
            sys.stderr.write(f"unexpected git cat-file header for {blob}: {header}\n")
            sys.exit(1)
        size = int(parts[2])
        data = out[offset:offset + size]
        offset += size
        if offset >= len(out) or out[offset:offset + 1] != b"\n":
            sys.stderr.write(f"missing git cat-file record separator for {blob}\n")
            sys.exit(1)
        offset += 1

        metadata = json.loads(data.decode("utf-8"))
        session_entries = []
        for session_index in sorted(sessions_by_checkpoint[checkpoint_path], key=lambda value: int(value)):
            session_prefix = f"/{checkpoint_path}/{session_index}"
            session_entries.append({
                "metadata": f"{session_prefix}/metadata.json",
                "transcript": f"{session_prefix}/full.jsonl",
                "content_hash": f"{session_prefix}/content_hash.txt",
                "prompt": f"{session_prefix}/prompt.txt",
            })
        metadata["sessions"] = session_entries

        rewritten_path = os.path.join(rewrite_dir, checkpoint_path, "metadata.json")
        os.makedirs(os.path.dirname(rewritten_path), exist_ok=True)
        with open(rewritten_path, "w", encoding="utf-8") as rewritten:
            json.dump(metadata, rewritten, indent=2)
            rewritten.write("\n")

        manifest.write(f"{rewritten_path}\t{target_path}\n")
        paths.write(rewritten_path + "\n")
PY

	if [[ ! -s "$rewrite_paths" ]]; then
		return
	fi

	git hash-object -w --stdin-paths < "$rewrite_paths" > "$rewrite_hashes"
	awk -F '\t' '
		NR == FNR {
			target[FNR] = $2
			next
		}
		{
			print "100644 blob " $1 "\t" target[FNR]
		}
	' "$rewrite_manifest" "$rewrite_hashes" >> "$plan_entries_file"
}

compute_plan_counts() {
	planned_checkpoints=$(wc -l < "$raw_checkpoint_ids_file" | tr -d '[:space:]')
	planned_sessions=$(wc -l < "$raw_sessions_file" | tr -d '[:space:]')
	planned_raw_transcripts=$(awk -F '\t' '$5 == "raw_transcript" { count++ } END { print count + 0 }' "$full_artifacts_file")
	missing_raw_checkpoints=$(awk -v raw_file="$raw_checkpoint_ids_file" '
		BEGIN {
			while ((getline line < raw_file) > 0) {
				if (line != "") {
					raw[line] = 1
				}
			}
			close(raw_file)
		}
		NF && !($1 in raw) {
			count++
		}
		END {
			print count + 0
		}
	' "$checkpoint_ids_file")
	missing_metadata_checkpoints=$(awk -F '\t' -v metadata_file="$main_metadata_file" '
		BEGIN {
			while ((getline line < metadata_file) > 0) {
				split(line, fields, "\t")
				if (fields[4] == "checkpoint_metadata") {
					have[fields[1]] = 1
				}
			}
			close(metadata_file)
		}
		NF && !($1 in have) {
			count++
		}
		END {
			print count + 0
		}
	' "$raw_checkpoint_ids_file")
	missing_metadata_sessions=$(awk -F '\t' -v metadata_file="$main_metadata_file" '
		BEGIN {
			while ((getline line < metadata_file) > 0) {
				split(line, fields, "\t")
				if (fields[4] == "session_metadata") {
					have[fields[1] "\t" fields[3]] = 1
				}
			}
			close(metadata_file)
		}
		NF {
			key = $1 "\t" $3
			if (!(key in have)) {
				count++
			}
		}
		END {
			print count + 0
		}
	' "$raw_sessions_file")
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		-h|--help)
			show_help
			exit 0
			;;
		--list)
			list_mode=true
			shift
			;;
		--dry-run)
			dry_run=true
			shift
			;;
		--apply)
			apply=true
			shift
			;;
		--since)
			[[ $# -ge 2 ]] || die "--since requires a commit"
			since_commit="$2"
			shift 2
			;;
		--repo)
			[[ $# -ge 2 ]] || die "--repo requires a path"
			repo_path="$2"
			shift 2
			;;
		--head)
			[[ $# -ge 2 ]] || die "--head requires a commit"
			head_commitish="$2"
			shift 2
			;;
		-*)
			die "unknown option: $1"
			;;
		*)
			[[ -z "$since_commit" ]] || die "too many commit arguments"
			since_commit="$1"
			shift
			;;
	esac
done

mode_count=0
[[ "$list_mode" == "true" ]] && mode_count=$((mode_count + 1))
[[ "$dry_run" == "true" ]] && mode_count=$((mode_count + 1))
[[ "$apply" == "true" ]] && mode_count=$((mode_count + 1))
if (( mode_count > 1 )); then
	die "--list, --dry-run, and --apply are mutually exclusive"
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/v2-to-v1.XXXXXX")
plan_entries_file="$tmp_dir/plan_entries"
checkpoint_commits_file="$tmp_dir/checkpoint_commits"
checkpoint_ids_file="$tmp_dir/checkpoint_ids"
checkpoint_paths_file="$tmp_dir/checkpoint_paths"
full_artifacts_file="$tmp_dir/full_artifacts"
raw_sessions_file="$tmp_dir/raw_sessions"
raw_checkpoint_ids_file="$tmp_dir/raw_checkpoint_ids"
main_metadata_file="$tmp_dir/main_metadata"
: > "$plan_entries_file"

if [[ -z "$repo_path" ]]; then
	printf 'Local repo path: ' >&2
	IFS= read -r repo_path
fi

[[ -n "$repo_path" ]] || die "a local repo path is required"
[[ -d "$repo_path" ]] || die "repo path does not exist or is not a directory: $repo_path"

if ! repo_root=$(git -C "$repo_path" rev-parse --show-toplevel 2>/dev/null); then
	die "not inside a git repository: $repo_path"
fi
cd "$repo_root"

if [[ -z "$since_commit" ]]; then
	printf 'Show v2 checkpoints newer than commit: ' >&2
	IFS= read -r since_commit
fi

[[ -n "$since_commit" ]] || die "a base commit is required"

if ! since_hash=$(git rev-parse --verify --quiet "${since_commit}^{commit}"); then
	die "commit not found: $since_commit"
fi

head_hash=""
if [[ -n "$head_commitish" ]]; then
	if ! head_hash=$(git rev-parse --verify --quiet "${head_commitish}^{commit}"); then
		die "history tip not found: $head_commitish"
	fi
	git merge-base --is-ancestor "$since_hash" "$head_hash" 2>/dev/null ||
		die "$since_commit is not an ancestor of $head_commitish"
fi

if [[ -n "$head_hash" ]]; then
	write_checkpoint_commit_index_between "$since_hash" "$head_hash" "$checkpoint_commits_file"
else
	write_checkpoint_commit_index_from_all_refs "$since_hash" "$checkpoint_commits_file"
fi
if [[ ! -s "$checkpoint_commits_file" ]]; then
	if [[ -n "$head_hash" ]]; then
		printf 'No %s trailers found in %s..%s\n' "$TRAILER_KEY" "$since_hash" "$head_hash"
	else
		printf 'No %s trailers found on local branches/remotes containing %s\n' "$TRAILER_KEY" "$since_hash"
	fi
	exit 0
fi
write_checkpoint_id_files

if [[ "$list_mode" == "true" ]]; then
	checkpoint_count=$(wc -l < "$checkpoint_ids_file" | tr -d '[:space:]')
	printf 'Checkpoints: %s\n' "$checkpoint_count"
	printf 'checkpoint_id\tcommit_ids\n'
	awk -F '\t' '
		NF >= 2 {
			if (!seen_checkpoint[$1]++) {
				order[++n] = $1
			}
			key = $1 SUBSEP $2
			if (!seen_pair[key]++) {
				commits[$1] = commits[$1] == "" ? $2 : commits[$1] " " $2
			}
		}
		END {
			for (i = 1; i <= n; i++) {
				print order[i] "\t" commits[order[i]]
			}
		}
	' "$checkpoint_commits_file"
	exit 0
fi

main_ref_available=false
if git show-ref --verify --quiet "$V2_MAIN_REF"; then
	main_ref_available=true
else
	warn "missing $V2_MAIN_REF; companion metadata paths will not be shown"
fi

full_refs=$(list_full_refs)
[[ -n "$full_refs" ]] || die "missing refs under $V2_FULL_REF_PREFIX; cannot locate raw transcripts"

printf 'Repository: %s\n' "$repo_root"
if [[ -n "$head_hash" ]]; then
	printf 'Scanning commits: %s..%s\n' "$since_hash" "$head_hash"
else
	printf 'Scanning commits: local branches/remotes containing %s\n' "$since_hash"
fi
if [[ "$main_ref_available" == "true" ]]; then
	printf 'Companion metadata ref: %s\n' "$V2_MAIN_REF"
fi
printf 'Full refs:\n'
printf '%s\n' "$full_refs" | sed 's/^/  /'
printf '\n'

write_full_artifact_index
write_main_metadata_index
append_checkpoint_metadata_plan_entries
compute_plan_counts

if [[ "$dry_run" != "true" ]]; then
	if (( missing_raw_checkpoints > 0 )); then
		warn "$missing_raw_checkpoints checkpoint trailer(s) do not have raw_transcript artifacts and will be skipped"
	fi
	if (( missing_metadata_checkpoints > 0 )); then
		warn "$missing_metadata_checkpoints checkpoint(s) with raw transcripts are missing companion checkpoint metadata"
	fi
	if (( missing_metadata_sessions > 0 )); then
		warn "$missing_metadata_sessions session(s) with raw transcripts are missing companion session metadata"
	fi
fi

if [[ "$dry_run" == "true" ]]; then
	while IFS= read -r checkpoint_id; do
		[[ -n "$checkpoint_id" ]] || continue

		checkpoint_path=$(checkpoint_to_path "$checkpoint_id")

		checkpoint_output=""

		if ! awk -F '\t' -v checkpoint_id="$checkpoint_id" '$1 == checkpoint_id { found = 1; exit } END { exit found ? 0 : 1 }' "$full_artifacts_file"; then
			warn "no raw_transcript artifacts found for checkpoint $checkpoint_id"
			continue
		fi
		checkpoint_output=$(awk -F '\t' -v checkpoint_id="$checkpoint_id" '
			$1 == checkpoint_id {
				full_checkpoint_key = $4 SUBSEP $2
				if (!seen_full_checkpoint[full_checkpoint_key]++) {
					print "  full checkpoint folder: " $4 ":" $2
				}
				session_key = $4 SUBSEP $2 SUBSEP $3
				if (!seen_session[session_key]++) {
					print "  full session folder: " $4 ":" $2 "/" $3
				}
				print "    raw artifact: " $4 ":" $6
			}
		' "$full_artifacts_file")

		if [[ "$main_ref_available" == "true" ]] && awk -F '\t' -v checkpoint_id="$checkpoint_id" '$1 == checkpoint_id && $4 == "checkpoint_metadata" { found = 1; exit } END { exit found ? 0 : 1 }' "$main_metadata_file"; then
			metadata_output=$(awk -F '\t' -v checkpoint_id="$checkpoint_id" -v main_ref="$V2_MAIN_REF" '
				$1 == checkpoint_id {
					if (!printed_folder++) {
						print "  companion metadata folder: " main_ref ":" $2
					}
					if ($4 == "checkpoint_metadata") {
						print "    checkpoint metadata: " main_ref ":" $5
					} else if ($4 == "session_metadata") {
						print "    session metadata: " main_ref ":" $5
					} else if ($4 == "prompt") {
						print "    prompt: " main_ref ":" $5
					}
				}
			' "$main_metadata_file")
			checkpoint_output="${checkpoint_output}"$'\n'"${metadata_output}"
		elif [[ "$main_ref_available" == "true" ]]; then
			warn "checkpoint $checkpoint_id has raw transcript artifacts but no companion metadata on $V2_MAIN_REF"
		fi

		printf 'checkpoint %s\n' "$checkpoint_id"
		printf '%s' "$checkpoint_output"
		printf '\n'
	done < "$checkpoint_ids_file"
fi

planned_entries=$(wc -l < "$plan_entries_file" | tr -d '[:space:]')
unique_planned_entries=$(write_unique_mktree_input "$plan_entries_file" | wc -l | tr -d '[:space:]')

if [[ "$dry_run" != "true" ]]; then
	printf 'Migration plan:\n'
	printf '  target ref: %s\n' "$V1_REF"
	printf '  checkpoints with raw transcripts: %s\n' "$planned_checkpoints"
	printf '  sessions with raw transcripts: %s\n' "$planned_sessions"
	printf '  raw transcript base files: %s\n' "$planned_raw_transcripts"
	printf '  planned v1 tree entries: %s (%s unique target paths)\n' "$planned_entries" "$unique_planned_entries"
	printf '  missing raw-transcript checkpoints: %s\n' "$missing_raw_checkpoints"
	printf '  missing companion checkpoint metadata: %s\n' "$missing_metadata_checkpoints"
	printf '  missing companion session metadata: %s\n' "$missing_metadata_sessions"
	printf '\n'
fi

if [[ "$apply" == "true" ]]; then
	if [[ "$unique_planned_entries" == "0" ]]; then
		die "nothing to migrate: no v1 tree entries were planned"
	fi

	old_ref_hash=""
	base_tree_hash=""
	parent_hash=""
	bulk_migration_hash=""
	rewriting_bulk_migration=false
	if git show-ref --verify --quiet "$V1_REF"; then
		old_ref_hash=$(git rev-parse "$V1_REF^{commit}")
		parent_hash="$old_ref_hash"
		base_tree_hash=$(git rev-parse "$old_ref_hash^{tree}")

		bulk_migration_hash=$(find_bulk_migration_ancestor "$old_ref_hash")
		if [[ -n "$bulk_migration_hash" ]]; then
			rewriting_bulk_migration=true
			if parent_hash=$(git rev-parse --verify --quiet "${bulk_migration_hash}^"); then
				base_tree_hash=$(git rev-parse "$parent_hash^{tree}")
			else
				parent_hash=""
				base_tree_hash=""
			fi
		fi
	fi

	source_plan_entries_file="$plan_entries_file"
	apply_checkpoint_ids_source_file="$tmp_dir/apply_checkpoint_ids_source"
	if [[ "$rewriting_bulk_migration" == "true" ]]; then
		source_plan_entries_file="$tmp_dir/bulk_plan_entries"
		write_bulk_migration_entries "$old_ref_hash" "$parent_hash" "$source_plan_entries_file" "$apply_checkpoint_ids_source_file"
	else
		write_apply_checkpoint_ids "$apply_checkpoint_ids_source_file"
	fi

	checkpoint_dates_file="$tmp_dir/checkpoint_dates"
	apply_checkpoint_ids_file="$tmp_dir/apply_checkpoint_ids"
	write_checkpoint_commit_dates "$source_plan_entries_file" "$checkpoint_dates_file"
	order_checkpoint_ids_by_date "$apply_checkpoint_ids_source_file" "$checkpoint_dates_file" "$apply_checkpoint_ids_file"

	commit_count=0
	current_tree_hash="$base_tree_hash"
	final_commit_hash="$parent_hash"
	while IFS= read -r checkpoint_id; do
		[[ -n "$checkpoint_id" ]] || continue
		checkpoint_entries_file="$tmp_dir/checkpoint-${checkpoint_id}.entries"
		write_checkpoint_entries "$checkpoint_id" "$checkpoint_entries_file" "$source_plan_entries_file"
		[[ -s "$checkpoint_entries_file" ]] || continue

		new_tree_hash=$(build_v1_tree_from_entries "$current_tree_hash" "$checkpoint_entries_file")
		if [[ -n "$current_tree_hash" && "$new_tree_hash" == "$current_tree_hash" ]]; then
			continue
		fi

		commit_date=$(checkpoint_commit_date "$checkpoint_id" "$checkpoint_dates_file")
		final_commit_hash=$(create_v1_checkpoint_migration_commit "$checkpoint_id" "$new_tree_hash" "$final_commit_hash" "$commit_date")
		current_tree_hash="$new_tree_hash"
		commit_count=$((commit_count + 1))
	done < "$apply_checkpoint_ids_file"

	if (( commit_count == 0 )); then
		printf '%s is already up to date; no migration commit created.\n' "$V1_REF"
		exit 0
	fi

	if [[ -n "$old_ref_hash" ]]; then
		git update-ref "$V1_REF" "$final_commit_hash" "$old_ref_hash"
	else
		git update-ref "$V1_REF" "$final_commit_hash"
	fi

	if [[ "$rewriting_bulk_migration" == "true" ]]; then
		printf 'Rewrote previous bulk migration into %s per-checkpoint commit(s).\n' "$commit_count"
	else
		printf 'Wrote %s per-checkpoint migration commit(s).\n' "$commit_count"
	fi
	printf 'Latest migration commit: %s\n' "$final_commit_hash"
	printf 'Updated %s\n' "$V1_REF"
	exit 0
fi

if [[ "$dry_run" != "true" ]]; then
	printf 'Plan only: no refs were written. Use --dry-run to print every source and target artifact, or --apply to write migration commits.\n'
fi
