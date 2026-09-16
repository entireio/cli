"""DatabricksSync - cross-session checkpoint analytics on Delta tables.

WHY THIS IS LOAD-BEARING, NOT STORAGE
-------------------------------------
`handoff` and `assess` answer questions about ONE checkpoint, and they answer
them entirely from local git. Databricks exists here to answer the questions
that only have meaning across the whole history of a project:

  * Is unresolved context accumulating or being discharged over time?
    (open_items_trend - a blocker recorded in checkpoint 1 and never answered
    by checkpoint 6 is invisible to any single-checkpoint view.)

  * Which files are risk hotspots - repeatedly touched across many separate
    sessions AND carrying many dependents?
    (file_churn_risk - churn is a property of the sequence, not of one commit.)

Delete this module and the product still runs, but every trend and ranking
degrades to a single point in time. That is the intended failure mode, and the
CLI states it explicitly rather than silently showing one checkpoint's numbers
as though they were a trend.

CREDENTIALS
-----------
Never committed. Resolved in this order:
  1. environment: DATABRICKS_SERVER_HOSTNAME / DATABRICKS_HTTP_PATH /
     DATABRICKS_TOKEN
  2. a gitignored .databricks.local.json in the repo root

EGRESS CONTRACT
---------------
This module is the ONLY code in Checkpoint Lens that opens a socket
(`_connect`, called by `sync` for writes and `_query` for reads). Everything it
sends is bound by one rule:

    Only DERIVED, NON-REVERSIBLE SIGNALS leave this machine: counts, kinds,
    enums, salted digests, scores, and checkpoint/commit identifiers. No raw
    prompt text and no transcript text is sent, at any length.

Truncation is NOT part of that rule and never was a safeguard. A prompt cut at
800 characters is still 800 characters of the user's own words; it is a size
cap wearing a privacy label. The previous schema shipped `sessions.intent` and
`decisions.text` verbatim-then-truncated, which put raw prompts, transcript
prose and local filesystem paths into an external warehouse. Both columns are
gone. `derive_signals` is the single chokepoint that replaced them, and
`assert_egress_safe` fails the write if anything text-shaped ever reaches a row
again.

What still leaves, deliberately: checkpoint metadata that is not prompt or
transcript content - `checkpoint_id`, `linked_commit`, `branch`, `session_id`,
`agent`, `model`, `created_at`, `file_path`, token counts, attribution
percentages. `file_churn` is unbuildable without `file_path`, and none of these
carry authored prose. That is a scope decision, stated so it can be revisited.

The full verbatim text is never lost: it stays in the local checkpoints, and
`handoff` / `drift` / `assess` keep rendering it from git. Only the aggregate
layer is de-identified.

DATA PROVENANCE
---------------
Every row is derived from this repository's own Entire Checkpoints, created by
the team during the build. No third-party, customer or personal data is
uploaded.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from dataclasses import dataclass, field
from typing import Any, Iterable

from .models import CheckpointRecord, has_redaction_marker
from .reader import attribute_to_first_appearance

CONFIG_FILENAME = ".databricks.local.json"

DEFAULT_CATALOG = "workspace"
DEFAULT_SCHEMA = "checkpoint_lens"

# Build artefacts, vendored code and lockfiles are not authored work. They
# churn constantly and would otherwise dominate a hotspot ranking that is
# supposed to point a reviewer at risky *source*. Our own history contains a
# committed-then-deleted __pycache__, which sat at the top of the ranking until
# this filter existed.
GENERATED_PATH = re.compile(
    r"(^|/)(__pycache__|node_modules|\.venv|venv|dist|build|vendor|target|"
    r"\.mypy_cache|\.pytest_cache|coverage)(/|$)"
    r"|\.(pyc|pyo|class|o|so|dll|exe|lock|min\.js|map)$"
    r"|(^|/)(package-lock\.json|yarn\.lock|go\.sum|poetry\.lock)$",
    re.IGNORECASE,
)


NEWLINE = re.compile("[\r\n]")


def is_generated_path(path: str) -> bool:
    """True for build output, vendored code and lockfiles."""
    return bool(GENERATED_PATH.search(path.replace("\\", "/")))


# --------------------------------------------------------------------------
# The egress boundary
# --------------------------------------------------------------------------
#
# Everything below exists to answer one question mechanically instead of by
# review: "can raw prompt or transcript text reach the wire?"

DIGEST_DOMAIN = "entire-lens/v1/checkpoint-signal"
DIGEST_CHARS = 16

# What a column is allowed to hold. This table is the schema of record for the
# boundary: `assert_egress_safe` validates every outgoing row against it, so a
# column added later cannot quietly reintroduce free text the way `intent` and
# `text` did.
KIND_COUNT = "count"      # int
KIND_SCORE = "score"      # float
KIND_FLAG = "flag"        # bool
KIND_DIGEST = "digest"    # salted sha256 prefix, or empty
KIND_ENUM = "enum"        # closed vocabulary; spaces allowed ("Claude Code")
KIND_IDENT = "ident"      # id / sha / branch: no whitespace at all
KIND_PATH = "path"        # repo-relative path; may contain spaces
KIND_TIME = "time"        # ISO-8601 timestamp

MAX_ENUM_CHARS = 40
MAX_IDENT_CHARS = 128
MAX_PATH_CHARS = 512
MAX_TIME_CHARS = 40

HEX16 = re.compile("^[0-9a-f]{16}$")

EGRESS_COLUMNS: dict[str, list[tuple[str, str]]] = {
    "checkpoint_sessions": [
        ("checkpoint_id", KIND_IDENT),
        ("session_index", KIND_COUNT),
        ("session_id", KIND_IDENT),
        ("created_at", KIND_TIME),
        ("branch", KIND_IDENT),
        ("agent", KIND_ENUM),
        ("model", KIND_ENUM),
        ("turn_count", KIND_COUNT),
        ("save_step_count", KIND_COUNT),
        ("prompt_count", KIND_COUNT),
        ("intent_source", KIND_ENUM),
        ("intent_len", KIND_COUNT),
        ("intent_word_count", KIND_COUNT),
        ("intent_digest", KIND_DIGEST),
        ("intent_redacted", KIND_FLAG),
        ("files_touched_count", KIND_COUNT),
        ("decision_count", KIND_COUNT),
        ("input_tokens", KIND_COUNT),
        ("output_tokens", KIND_COUNT),
        ("cache_read_tokens", KIND_COUNT),
        ("cache_creation_tokens", KIND_COUNT),
        ("api_call_count", KIND_COUNT),
        ("total_tokens", KIND_COUNT),
        ("agent_percentage", KIND_SCORE),
        ("total_lines_changed", KIND_COUNT),
        ("linked_commit", KIND_IDENT),
        ("repo", KIND_IDENT),
    ],
    "checkpoint_files": [
        ("checkpoint_id", KIND_IDENT),
        ("created_at", KIND_TIME),
        ("file_path", KIND_PATH),
        ("repo", KIND_IDENT),
    ],
    "checkpoint_decisions": [
        ("checkpoint_id", KIND_IDENT),
        ("created_at", KIND_TIME),
        ("kind", KIND_ENUM),
        ("source", KIND_ENUM),
        ("confidence", KIND_ENUM),
        ("text_len", KIND_COUNT),
        ("text_word_count", KIND_COUNT),
        ("text_digest", KIND_DIGEST),
        ("text_redacted", KIND_FLAG),
        ("repo", KIND_IDENT),
    ],
}


class EgressViolation(RuntimeError):
    """A row carried a value the egress contract does not permit.

    Raised before any statement is executed. Failing the sync is the correct
    outcome: an under-redacted upload cannot be un-sent, and a sync that did
    not happen costs a trend chart.
    """


def _check_value(table: str, column: str, kind: str, value: Any) -> None:
    if value is None:
        return
    if kind == KIND_FLAG:
        if not isinstance(value, bool):
            raise EgressViolation(
                "%s.%s must be a bool, got %r" % (table, column, type(value))
            )
        return
    if kind == KIND_COUNT:
        if isinstance(value, bool) or not isinstance(value, int):
            raise EgressViolation(
                "%s.%s must be an int, got %r" % (table, column, type(value))
            )
        return
    if kind == KIND_SCORE:
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise EgressViolation(
                "%s.%s must be numeric, got %r" % (table, column, type(value))
            )
        return

    if not isinstance(value, str):
        raise EgressViolation(
            "%s.%s must be a string, got %r" % (table, column, type(value))
        )

    # No permitted column may span lines. Prose does; an id, a path and an
    # enum do not. This is the cheapest tripwire for "someone put a transcript
    # in here".
    if NEWLINE.search(value):
        raise EgressViolation(
            "%s.%s contains a newline - free text is not permitted" % (table, column)
        )

    if kind == KIND_DIGEST:
        if value and not HEX16.match(value):
            raise EgressViolation(
                "%s.%s is not a %d-char digest" % (table, column, DIGEST_CHARS)
            )
        return

    cap = {
        KIND_ENUM: MAX_ENUM_CHARS,
        KIND_IDENT: MAX_IDENT_CHARS,
        KIND_PATH: MAX_PATH_CHARS,
        KIND_TIME: MAX_TIME_CHARS,
    }[kind]
    if len(value) > cap:
        raise EgressViolation(
            "%s.%s is %d chars, over the %d-char cap for %s - free text is not permitted"
            % (table, column, len(value), cap, kind)
        )
    if kind == KIND_IDENT and any(c.isspace() for c in value):
        raise EgressViolation(
            "%s.%s contains whitespace and is not an identifier" % (table, column)
        )


def assert_egress_safe(table: str, rows: list[list[Any]]) -> None:
    """Validate every outgoing row against EGRESS_COLUMNS, or refuse to send.

    Called immediately before the INSERT rather than at row-construction time,
    so a future caller that assembles rows some other way is still covered.
    """
    spec = EGRESS_COLUMNS.get(table)
    if spec is None:
        raise EgressViolation("no egress column spec for table %r" % table)
    for row in rows:
        if len(row) != len(spec):
            raise EgressViolation(
                "%s row has %d values, spec has %d columns" % (table, len(row), len(spec))
            )
        for (column, kind), value in zip(spec, row):
            _check_value(table, column, kind, value)


def derive_signals(text: str, repo_key: str) -> dict[str, Any]:
    """Turn free text into the only things allowed to describe it externally.

    Returns length, word count, a salted digest, and whether Entire's redaction
    pipeline had already removed content from it. The text itself is discarded
    here and is never returned.

    On the digest, stated honestly: it is a stable key for dedupe and joins,
    NOT an anonymisation primitive. A short, low-entropy string can be
    recovered from any hash by guessing it. Salting with the repo key gives
    domain separation so digests cannot be correlated across repositories, and
    the plaintext never leaves this machine either way - that, not the hash, is
    what makes this safe.
    """
    text = text or ""
    normalized = " ".join(text.split())
    digest = ""
    if normalized:
        h = hashlib.sha256()
        h.update(DIGEST_DOMAIN.encode("utf-8"))
        h.update(b"\x00")
        h.update((repo_key or "").encode("utf-8"))
        h.update(b"\x00")
        h.update(normalized.encode("utf-8"))
        digest = h.hexdigest()[:DIGEST_CHARS]
    return {
        "len": len(text),
        "word_count": len(normalized.split()) if normalized else 0,
        "digest": digest,
        "redacted": has_redaction_marker(text),
    }


@dataclass
class DatabricksConfig:
    server_hostname: str = ""
    http_path: str = ""
    access_token: str = ""
    catalog: str = DEFAULT_CATALOG
    schema: str = DEFAULT_SCHEMA

    @property
    def configured(self) -> bool:
        return bool(self.server_hostname and self.http_path and self.access_token)

    @property
    def fq_schema(self) -> str:
        return "%s.%s" % (self.catalog, self.schema)

    def table(self, name: str) -> str:
        return "%s.%s" % (self.fq_schema, name)

    @classmethod
    def resolve(cls, repo: str = ".") -> "DatabricksConfig":
        cfg = cls()
        path = os.path.join(repo, CONFIG_FILENAME)
        if os.path.isfile(path):
            try:
                with open(path, "r", encoding="utf-8") as fh:
                    data = json.load(fh)
                cfg.server_hostname = str(data.get("server_hostname", "") or "")
                cfg.http_path = str(data.get("http_path", "") or "")
                cfg.access_token = str(data.get("access_token", "") or "")
                cfg.catalog = str(data.get("catalog", "") or DEFAULT_CATALOG)
                cfg.schema = str(data.get("schema", "") or DEFAULT_SCHEMA)
            except (OSError, json.JSONDecodeError, ValueError):
                pass
        # Environment wins: it is the path CI and other machines use.
        cfg.server_hostname = os.environ.get("DATABRICKS_SERVER_HOSTNAME", cfg.server_hostname)
        cfg.http_path = os.environ.get("DATABRICKS_HTTP_PATH", cfg.http_path)
        cfg.access_token = os.environ.get("DATABRICKS_TOKEN", cfg.access_token)
        cfg.catalog = os.environ.get("DATABRICKS_CATALOG", cfg.catalog)
        cfg.schema = os.environ.get("DATABRICKS_SCHEMA", cfg.schema)
        return cfg


@dataclass
class SyncResult:
    ok: bool = False
    sessions_written: int = 0
    files_written: int = 0
    decisions_written: int = 0
    error: str = ""
    detail: list[str] = field(default_factory=list)


@dataclass
class AggregateResult:
    """An aggregate answered by Databricks. `ok=False` means the number is
    genuinely unknown - callers must render that as unavailable, never as
    zero."""

    name: str
    ok: bool
    rows: list[dict[str, Any]] = field(default_factory=list)
    error: str = ""
    sql: str = ""


DDL_SESSIONS = """
CREATE TABLE IF NOT EXISTS {t} (
  checkpoint_id       STRING,
  session_index       INT,
  session_id          STRING,
  created_at          STRING,
  branch              STRING,
  agent               STRING,
  model               STRING,
  turn_count          INT,
  save_step_count     INT,
  prompt_count        INT,
  intent_source       STRING,
  intent_len          INT,
  intent_word_count   INT,
  intent_digest       STRING,
  intent_redacted     BOOLEAN,
  files_touched_count INT,
  decision_count      INT,
  input_tokens        BIGINT,
  output_tokens       BIGINT,
  cache_read_tokens   BIGINT,
  cache_creation_tokens BIGINT,
  api_call_count      INT,
  total_tokens        BIGINT,
  agent_percentage    DOUBLE,
  total_lines_changed INT,
  linked_commit       STRING,
  repo                STRING
) USING DELTA
"""

DDL_FILES = """
CREATE TABLE IF NOT EXISTS {t} (
  checkpoint_id STRING,
  created_at    STRING,
  file_path     STRING,
  repo          STRING
) USING DELTA
"""

DDL_DECISIONS = """
CREATE TABLE IF NOT EXISTS {t} (
  checkpoint_id STRING,
  created_at    STRING,
  kind          STRING,
  source        STRING,
  confidence    STRING,
  text_len      INT,
  text_word_count INT,
  text_digest   STRING,
  text_redacted BOOLEAN,
  repo          STRING
) USING DELTA
"""

# Unresolved-context debt over time. A single checkpoint cannot answer this.
SQL_OPEN_ITEMS_TREND = """
SELECT
  d.checkpoint_id,
  MIN(d.created_at)                                            AS created_at,
  SUM(CASE WHEN d.kind = 'blocker'       THEN 1 ELSE 0 END)     AS blockers,
  SUM(CASE WHEN d.kind = 'open_question' THEN 1 ELSE 0 END)     AS open_questions,
  SUM(CASE WHEN d.kind = 'risk'          THEN 1 ELSE 0 END)     AS risks,
  SUM(CASE WHEN d.kind IN ('blocker','open_question','risk')
           THEN 1 ELSE 0 END)                                   AS unresolved_total
FROM {decisions} d
WHERE d.repo = ?
GROUP BY d.checkpoint_id
ORDER BY created_at
"""

# Churn hotspots: a property of the sequence of checkpoints, not of any one.
SQL_FILE_CHURN = """
SELECT
  f.file_path,
  COUNT(DISTINCT f.checkpoint_id) AS checkpoints_touching,
  MIN(f.created_at)               AS first_seen,
  MAX(f.created_at)               AS last_seen
FROM {files} f
WHERE f.repo = ?
GROUP BY f.file_path
HAVING COUNT(DISTINCT f.checkpoint_id) >= 1
ORDER BY checkpoints_touching DESC, f.file_path
LIMIT 15
"""

# Headline coverage numbers surfaced by handoff and drift.
#
# Note what this reads: intent_source, never the intent itself. That was
# already true before the intent column was removed, which is why removing it
# cost no aggregate anything.
#
# decisions_captured counts rows in the decisions table, NOT SUM(decision_count)
# from the sessions table. The two disagree on purpose: decision_count is the
# raw per-session count, while the decisions table is deduplicated to first
# appearance. Reporting the raw sum next to a deduplicated table showed a
# headline number no query against the data could reproduce.
SQL_COVERAGE = """
SELECT
  (SELECT COUNT(DISTINCT checkpoint_id) FROM {sessions} WHERE repo = ?) AS checkpoints,
  (SELECT COUNT(*) FROM {decisions} WHERE repo = ?)                     AS decisions_captured,
  (SELECT ROUND(AVG(agent_percentage), 1) FROM {sessions} WHERE repo = ?) AS avg_agent_pct,
  (SELECT SUM(total_lines_changed) FROM {sessions} WHERE repo = ?)      AS lines_changed,
  (SELECT SUM(total_tokens) FROM {sessions} WHERE repo = ?)             AS tokens,
  (SELECT ROUND(
      100.0 * SUM(CASE WHEN intent_source <> 'unavailable' THEN 1 ELSE 0 END)
      / NULLIF(COUNT(*), 0), 1)
   FROM {sessions} WHERE repo = ?)                                      AS intent_coverage_pct
"""


class DatabricksSync:
    def __init__(self, repo: str = ".", config: DatabricksConfig | None = None) -> None:
        self.repo = repo
        self.config = config or DatabricksConfig.resolve(repo)
        self._repo_key = os.path.basename(os.path.abspath(repo)) or "repo"

    # ---------------- availability ----------------

    @staticmethod
    def connector_available() -> bool:
        try:
            import databricks.sql  # noqa: F401
        except ImportError:
            return False
        return True

    def unavailable_reason(self) -> str:
        if not self.connector_available():
            return (
                "databricks-sql-connector is not installed "
                "(pip install databricks-sql-connector)"
            )
        if not self.config.configured:
            return (
                "no Databricks credentials found - set DATABRICKS_SERVER_HOSTNAME / "
                "DATABRICKS_HTTP_PATH / DATABRICKS_TOKEN, or create a gitignored "
                + CONFIG_FILENAME
            )
        return ""

    def _connect(self):
        import databricks.sql

        return databricks.sql.connect(
            server_hostname=self.config.server_hostname,
            http_path=self.config.http_path,
            access_token=self.config.access_token,
        )

    # ---------------- schema ----------------

    def ensure_schema(self, cursor) -> None:
        cursor.execute("CREATE SCHEMA IF NOT EXISTS " + self.config.fq_schema)
        cursor.execute(DDL_SESSIONS.format(t=self.config.table("checkpoint_sessions")))
        cursor.execute(DDL_FILES.format(t=self.config.table("checkpoint_files")))
        cursor.execute(DDL_DECISIONS.format(t=self.config.table("checkpoint_decisions")))

    # ---------------- purge ----------------

    def purge_statements(self) -> list[str]:
        """The exact statements `purge` will execute, for review before it runs.

        DROP, not DELETE, and the distinction is the whole point. The old
        schema had `sessions.intent` and `decisions.text` columns holding raw
        prompt and transcript text; deleting rows leaves those columns in
        place, and Delta keeps the deleted rows reachable through time travel
        until a VACUUM past the retention window. Dropping the table discards
        the schema and its history together, and the tables are recreated
        empty on the new, text-free schema by the sync that follows.

        Consequence worth stating plainly: DROP is not scoped by `repo`, so it
        removes every repo's rows from this schema, not just ours. The column
        removal is a schema change and cannot be scoped to one repo anyway.
        Re-run `sync` in any other repo sharing this workspace afterwards.
        """
        return [
            "DROP TABLE IF EXISTS " + self.config.table(t)
            for t in ("checkpoint_sessions", "checkpoint_files", "checkpoint_decisions")
        ]

    def purge(self) -> SyncResult:
        """Drop the tables that held raw text. Recreation is left to `sync`."""
        reason = self.unavailable_reason()
        if reason:
            return SyncResult(ok=False, error=reason)
        result = SyncResult()
        try:
            with self._connect() as conn:
                with conn.cursor() as cur:
                    for statement in self.purge_statements():
                        cur.execute(statement)
                        result.detail.append(statement)
            result.ok = True
        except Exception as exc:  # noqa: BLE001
            result.ok = False
            result.error = "%s: %s" % (type(exc).__name__, exc)
        return result

    # ---------------- write ----------------

    def build_rows(self, records: Iterable[CheckpointRecord]) -> dict[str, list[list[Any]]]:
        """Every value that would leave this machine, and nothing else.

        Split out of `sync` so that `--dry-run` shows the ACTUAL outgoing rows
        rather than a description of them. A manifest assembled by separate
        code would be a second implementation to keep honest, and the whole
        point of the manifest is that it can be trusted.
        """
        records = list(records)
        session_rows: list[list[Any]] = []
        file_rows: list[list[Any]] = []
        decision_rows: list[list[Any]] = []

        # Attribute each decision to the checkpoint where it FIRST appeared.
        # Entire stores the whole compacted session in every checkpoint, so
        # raw occurrences would count one blocker once per later commit and
        # make the trend monotonically increasing - the opposite of what it is
        # meant to show.
        first_seen = attribute_to_first_appearance(records)

        for rec in records:
            for path in rec.files_touched:
                if is_generated_path(path):
                    continue
                file_rows.append(
                    [rec.checkpoint_id, rec.created_at, path, self._repo_key]
                )
            for d in first_seen.get(rec.checkpoint_id, []):
                # d.text stays on this machine. Only its shape travels: kind,
                # source, confidence, size and a salted digest. The trend
                # aggregate groups on `kind` and never read the prose.
                sig = derive_signals(d.text, self._repo_key)
                decision_rows.append(
                    [
                        rec.checkpoint_id,
                        rec.created_at,
                        d.kind,
                        d.source,
                        d.confidence,
                        sig["len"],
                        sig["word_count"],
                        sig["digest"],
                        sig["redacted"],
                        self._repo_key,
                    ]
                )
            for s in rec.sessions:
                row = s.to_row()
                # The stated intent is the user's own prompt. Its source and
                # size are analytically useful; its words are not ours to
                # upload, and the coverage aggregate only ever tested
                # intent_source.
                isig = derive_signals(row.get("intent", ""), self._repo_key)
                session_rows.append(
                    [
                        row["checkpoint_id"],
                        row["session_index"],
                        row["session_id"],
                        row["created_at"],
                        row["branch"],
                        row["agent"],
                        row["model"],
                        row["turn_count"],
                        row["save_step_count"],
                        row["prompt_count"],
                        row["intent_source"],
                        isig["len"],
                        isig["word_count"],
                        isig["digest"],
                        isig["redacted"],
                        row["files_touched_count"],
                        row["decision_count"],
                        row["input_tokens"],
                        row["output_tokens"],
                        row["cache_read_tokens"],
                        row["cache_creation_tokens"],
                        row["api_call_count"],
                        row["total_tokens"],
                        row["agent_percentage"],
                        row["total_lines_changed"],
                        rec.linked_commit,
                        self._repo_key,
                    ]
                )

        built = {
            "checkpoint_sessions": session_rows,
            "checkpoint_files": file_rows,
            "checkpoint_decisions": decision_rows,
        }
        # Validate here too, so `--dry-run` fails on the same violation a real
        # sync would rather than printing a manifest of rows that would be
        # refused a second later.
        for table, rows in built.items():
            assert_egress_safe(table, rows)
        return built

    def sync(self, records: Iterable[CheckpointRecord]) -> SyncResult:
        """Replace this repo's rows with the current checkpoint history.

        Idempotent by design: a DELETE for this repo followed by inserts, so
        re-running after new checkpoints never double-counts. The repo key
        scopes every statement, so two projects can share one workspace.
        """
        reason = self.unavailable_reason()
        if reason:
            return SyncResult(ok=False, error=reason)

        records = list(records)
        result = SyncResult()
        try:
            with self._connect() as conn:
                with conn.cursor() as cur:
                    self.ensure_schema(cur)

                    for table in ("checkpoint_sessions", "checkpoint_files", "checkpoint_decisions"):
                        cur.execute(
                            "DELETE FROM %s WHERE repo = ?" % self.config.table(table),
                            [self._repo_key],
                        )

                    built = self.build_rows(records)
                    session_rows = built["checkpoint_sessions"]
                    file_rows = built["checkpoint_files"]
                    decision_rows = built["checkpoint_decisions"]

                    result.sessions_written = self._insert(
                        cur, "checkpoint_sessions", session_rows
                    )
                    result.files_written = self._insert(cur, "checkpoint_files", file_rows)
                    result.decisions_written = self._insert(
                        cur, "checkpoint_decisions", decision_rows
                    )
            result.ok = True
        except Exception as exc:  # noqa: BLE001 - surface the real driver error
            result.ok = False
            result.error = "%s: %s" % (type(exc).__name__, exc)
        return result

    def _insert(self, cursor, table: str, rows: list[list[Any]]) -> int:
        """Send rows for one table.

        The egress check runs here, immediately before the statement, so it
        covers every caller including any added later. A violation raises
        rather than dropping the offending value: silently shipping a row
        minus one column would hide the very mistake this catches.
        """
        if not rows:
            return 0
        assert_egress_safe(table, rows)
        ncols = len(EGRESS_COLUMNS[table])
        placeholder = "(" + ",".join(["?"] * ncols) + ")"
        written = 0
        # Batched multi-row INSERT: one 2X-Small serverless warehouse is the
        # whole compute budget, so statement count is the thing to economise.
        batch = 50
        for i in range(0, len(rows), batch):
            chunk = rows[i : i + batch]
            sql = "INSERT INTO %s VALUES %s" % (
                self.config.table(table),
                ",".join([placeholder] * len(chunk)),
            )
            flat: list[Any] = []
            for r in chunk:
                flat.extend(r)
            cursor.execute(sql, flat)
            written += len(chunk)
        return written

    # ---------------- read ----------------

    def _query(self, name: str, sql: str, params: int = 1) -> AggregateResult:
        reason = self.unavailable_reason()
        if reason:
            return AggregateResult(name=name, ok=False, error=reason, sql=sql)
        try:
            with self._connect() as conn:
                with conn.cursor() as cur:
                    cur.execute(sql, [self._repo_key] * params)
                    cols = [d[0] for d in cur.description]
                    rows = [dict(zip(cols, r)) for r in cur.fetchall()]
            return AggregateResult(name=name, ok=True, rows=rows, sql=sql)
        except Exception as exc:  # noqa: BLE001
            return AggregateResult(
                name=name, ok=False, error="%s: %s" % (type(exc).__name__, exc), sql=sql
            )

    def coverage(self) -> AggregateResult:
        # Six scalar subqueries, so six bound repo parameters.
        return self._query(
            "coverage",
            SQL_COVERAGE.format(
                sessions=self.config.table("checkpoint_sessions"),
                decisions=self.config.table("checkpoint_decisions"),
            ),
            params=6,
        )

    def open_items_trend(self) -> AggregateResult:
        return self._query(
            "open_items_trend",
            SQL_OPEN_ITEMS_TREND.format(decisions=self.config.table("checkpoint_decisions")),
        )

    def file_churn(self) -> AggregateResult:
        return self._query(
            "file_churn", SQL_FILE_CHURN.format(files=self.config.table("checkpoint_files"))
        )
