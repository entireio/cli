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

DATA PROVENANCE
---------------
Every row is derived from this repository's own Entire Checkpoints, created by
the team during the build. No third-party, customer or personal data is
uploaded. Prompt and decision text is authored by the team and its agent.
Free-text columns are truncated before upload.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from typing import Any, Iterable

from .models import CheckpointRecord

CONFIG_FILENAME = ".databricks.local.json"

DEFAULT_CATALOG = "workspace"
DEFAULT_SCHEMA = "checkpoint_lens"

# Free-text is truncated before it leaves the machine: these tables exist for
# aggregation, not for rehosting transcripts.
MAX_TEXT = 800


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
  intent              STRING,
  intent_source       STRING,
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
  speaker       STRING,
  text          STRING,
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

# Headline coverage number surfaced by handoff and drift.
SQL_COVERAGE = """
SELECT
  COUNT(DISTINCT s.checkpoint_id)                          AS checkpoints,
  SUM(s.decision_count)                                    AS decisions_captured,
  ROUND(AVG(s.agent_percentage), 1)                        AS avg_agent_pct,
  SUM(s.total_lines_changed)                               AS lines_changed,
  SUM(s.total_tokens)                                      AS tokens,
  ROUND(
    100.0 * SUM(CASE WHEN s.intent_source <> 'unavailable' THEN 1 ELSE 0 END)
    / NULLIF(COUNT(*), 0), 1)                              AS intent_coverage_pct
FROM {sessions} s
WHERE s.repo = ?
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

    # ---------------- write ----------------

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

                    session_rows: list[list[Any]] = []
                    file_rows: list[list[Any]] = []
                    decision_rows: list[list[Any]] = []

                    for rec in records:
                        for path in rec.files_touched:
                            file_rows.append(
                                [rec.checkpoint_id, rec.created_at, path, self._repo_key]
                            )
                        for d in rec.all_decisions():
                            decision_rows.append(
                                [
                                    rec.checkpoint_id,
                                    rec.created_at,
                                    d.kind,
                                    d.speaker,
                                    d.text[:MAX_TEXT],
                                    self._repo_key,
                                ]
                            )
                        for s in rec.sessions:
                            row = s.to_row()
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
                                    (row["intent"] or "")[:MAX_TEXT],
                                    row["intent_source"],
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

                    result.sessions_written = self._insert(
                        cur, "checkpoint_sessions", 24, session_rows
                    )
                    result.files_written = self._insert(cur, "checkpoint_files", 4, file_rows)
                    result.decisions_written = self._insert(
                        cur, "checkpoint_decisions", 6, decision_rows
                    )
            result.ok = True
        except Exception as exc:  # noqa: BLE001 - surface the real driver error
            result.ok = False
            result.error = "%s: %s" % (type(exc).__name__, exc)
        return result

    def _insert(self, cursor, table: str, ncols: int, rows: list[list[Any]]) -> int:
        if not rows:
            return 0
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

    def _query(self, name: str, sql: str) -> AggregateResult:
        reason = self.unavailable_reason()
        if reason:
            return AggregateResult(name=name, ok=False, error=reason, sql=sql)
        try:
            with self._connect() as conn:
                with conn.cursor() as cur:
                    cur.execute(sql, [self._repo_key])
                    cols = [d[0] for d in cur.description]
                    rows = [dict(zip(cols, r)) for r in cur.fetchall()]
            return AggregateResult(name=name, ok=True, rows=rows, sql=sql)
        except Exception as exc:  # noqa: BLE001
            return AggregateResult(
                name=name, ok=False, error="%s: %s" % (type(exc).__name__, exc), sql=sql
            )

    def coverage(self) -> AggregateResult:
        return self._query(
            "coverage", SQL_COVERAGE.format(sessions=self.config.table("checkpoint_sessions"))
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
