package spaudit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
)

// MarkdownOptions tunes the generated document.
type MarkdownOptions struct {
	// Timestamp writes the generation time into the document.
	//
	// Off is the right choice for a file kept under version control: with a
	// timestamp, every run produces a diff even when nothing changed, and a CI
	// step that checks the inventory is current can never distinguish "stale"
	// from "regenerated a minute later". Facts change the file; running the
	// tool should not.
	Timestamp bool
	// Diffs embeds the unified diff for each differing procedure.
	Diffs bool
	// MaxDiffLines truncates an embedded diff. Zero means 40.
	MaxDiffLines int
}

// Markdown renders the report as the INVENTORY.md document.
func (rep *Report) Markdown(opts MarkdownOptions) string {
	if opts.MaxDiffLines <= 0 {
		opts.MaxDiffLines = 40
	}
	var b strings.Builder

	b.WriteString("# Stored Procedure 盤點\n\n")
	b.WriteString("> ⚠ 本檔由 `hrm-sql-mcp sp audit --format markdown` 產生，請勿手動編輯。\n")
	if rep.Offline {
		b.WriteString("> **這是離線盤點：只比對了兩方**——`Stored Procedure/` 原始檔 × Java 呼叫點。\n")
		b.WriteString("> ⚠ **資料庫沒有被查詢過。** 因此「哪些已部署」「檔案是不是實際在跑的版本」" +
			"「有沒有無主的程序在線上」這三個問題，本報告一律沒有回答，也不該被讀成沒有問題。" +
			"完整結論要在連得到資料庫的機器上重跑一次。\n\n")
	} else {
		b.WriteString("> 三方比對：`Stored Procedure/` 原始檔 × 資料庫 `sys.sql_modules` × Java 呼叫點。\n")
		b.WriteString("> ⚠ 結論只對下表這一份快照成立。測試快照彼此只差在時間點，都不是正式環境的完整還原。\n\n")
	}

	b.WriteString("| 項目 | 值 |\n| :--- | :--- |\n")
	if rep.Offline {
		b.WriteString("| 比對範圍 | 離線（**未查詢資料庫**） |\n")
	}
	for _, k := range sortedKeys(rep.Target) {
		fmt.Fprintf(&b, "| %s | `%s` |\n", targetLabel(k), rep.Target[k])
	}
	if rep.Snapshot != "" {
		fmt.Fprintf(&b, "| 資料快照 | %s |\n", rep.Snapshot)
	}
	fmt.Fprintf(&b, "| 原始檔目錄 | `%s` |\n", rep.SPDir)
	fmt.Fprintf(&b, "| Java 來源 | `%s`（%d 個檔案） |\n", rep.JavaDir, rep.JavaFiles)
	if opts.Timestamp {
		fmt.Fprintf(&b, "| 產生時間 | %s |\n", rep.Generated.Format("2006-01-02 15:04:05 -0700"))
	}
	b.WriteString("\n## 摘要\n\n| 狀態 | 數量 | 意義 |\n| :--- | ---: | :--- |\n")
	for _, s := range rep.Statuses() {
		fmt.Fprintf(&b, "| `%s` | %d | %s |\n", s, rep.Counts[s], Explain(s))
	}
	fmt.Fprintf(&b, "| **合計** | **%d** | |\n", len(rep.Rows))

	writeNotes(&b, rep)

	for _, s := range rep.Statuses() {
		rows := rep.ByStatus(s)
		if len(rows) == 0 {
			continue
		}
		writeSection(&b, s, rows, opts)
	}

	writeProblems(&b, rep)
	return b.String()
}

// writeNotes renders the human findings, immediately after the summary.
//
// Placed before the status tables on purpose. A note exists precisely because
// the derived status is misleading on its own, so it has to be read first —
// put at the bottom it would only ever be found by someone who already knew to
// look, which is the person who least needs it.
//
// Each note carries the status the audit computed for that procedure, so the
// two readings sit side by side rather than the reader having to go hunting
// for the row. The status is shown, never overwritten: the note is extra
// evidence, not a correction of the count.
func writeNotes(b *strings.Builder, rep *Report) {
	if len(rep.Notes) == 0 {
		return
	}

	statusOf := make(map[string]Status, len(rep.Rows))
	for _, r := range rep.Rows {
		statusOf[strings.ToLower(r.Name)] = r.Status
	}

	b.WriteString("\n## 📌 人工註記\n\n")
	b.WriteString("> 以下是靠查證得來、三方比對看不出來的結論。" +
		"註記**不會**改變任何狀態或數量——它們是額外的證據，不是對盤點結果的修正。\n\n")

	for _, name := range rep.Notes.Names() {
		st, ok := statusOf[name]
		label := "（本次盤點未出現此程序）"
		if ok {
			label = "本次盤點狀態：`" + string(st) + "`"
		}
		fmt.Fprintf(b, "### `%s`\n\n%s\n\n", name, label)
		for _, line := range strings.Split(rep.Notes.Get(name), "\n") {
			fmt.Fprintf(b, "> %s\n", strings.TrimRight(line, " \t"))
		}
		b.WriteString("\n")
	}
}

func writeSection(b *strings.Builder, s Status, rows []Row, opts MarkdownOptions) {
	heading := "## `" + string(s) + "` — " + Explain(s)
	switch s {
	case StatusGhost:
		heading = "## ⚠ `ghost` — " + Explain(s)
	case StatusMissingScript:
		heading = "## ⚠ `missing-script` — " + Explain(s)
	}
	fmt.Fprintf(b, "\n%s\n\n", heading)

	// The identical list is long and, by definition, uninteresting. Folding it
	// keeps the document skimmable without dropping the evidence that the
	// audit looked at those procedures too. scripted is its offline twin.
	fold := (s == StatusIdentical || s == StatusScripted) && len(rows) > 20
	if fold {
		fmt.Fprintf(b, "<details><summary>展開 %d 筆</summary>\n\n", len(rows))
	}

	switch s {
	case StatusGhost:
		b.WriteString("| 程序 | 有原始檔 | Java 呼叫點 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.Name, yesNo(r.InFile), sites(r.Calls(), 3))
		}
	case StatusDBOnly:
		b.WriteString("| 程序 | 資料庫最後修改 | Java 引用 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.DBName, ts(r.DBModified), sites(r.CallSites, 2))
		}
	case StatusFileOnly:
		b.WriteString("| 程序 | 原始檔 | 編碼 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.Name, filePath(r), r.FileEncoding)
		}
	case StatusDiffers:
		b.WriteString("| 程序 | 原始檔 | 資料庫最後修改 | Java 引用 |\n| :--- | :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n",
				r.Name, filePath(r), ts(r.DBModified), refCount(r))
		}
		if opts.Diffs {
			for _, r := range rows {
				fmt.Fprintf(b, "\n<details><summary><code>%s</code> 差異</summary>\n\n```diff\n%s```\n\n</details>\n",
					r.Name, clampDiff(r.Diff, opts.MaxDiffLines))
			}
		}
	case StatusUnreadable:
		b.WriteString("| 程序 | 原始檔 | 資料庫最後修改 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.Name, filePath(r), ts(r.DBModified))
		}
	case StatusMissingScript:
		b.WriteString("| 程序 | Java 呼叫點 |\n| :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s |\n", r.Name, sites(r.Calls(), 3))
		}
	case StatusUnreferenced:
		b.WriteString("| 程序 | 原始檔 | 編碼 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.Name, filePath(r), r.FileEncoding)
		}
	case StatusScripted:
		// No database column: offline there is nothing true to put in one, and
		// an empty column reads as "never deployed" rather than "not asked".
		b.WriteString("| 程序 | 原始檔 | Java 引用 |\n| :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", r.Name, filePath(r), refCount(r))
		}
	default: // orphan, identical
		b.WriteString("| 程序 | 原始檔 | 資料庫最後修改 | Java 引用 |\n| :--- | :--- | :--- | :--- |\n")
		for _, r := range rows {
			fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n",
				r.Name, filePath(r), ts(r.DBModified), refCount(r))
		}
	}

	if fold {
		b.WriteString("\n</details>\n")
	}
}

// writeProblems reports everything that makes the audit itself less than
// complete. It is at the bottom but it is not a footnote: a report with
// undecodable files is a partial report, and a reader who does not know that
// will read "no ghosts" as an all-clear.
func writeProblems(b *strings.Builder, rep *Report) {
	if len(rep.FileFailures) == 0 && len(rep.JavaFailures) == 0 &&
		len(rep.DuplicateFiles) == 0 && len(rep.DuplicateDBNames) == 0 {
		return
	}
	b.WriteString("\n## 掃描本身的問題\n\n")
	b.WriteString("以下項目讓這份報告不完整，結論要打折扣。\n\n")

	if len(rep.FileFailures) > 0 {
		b.WriteString("### 無法解碼的原始檔\n\n")
		for _, f := range sortedKeys(rep.FileFailures) {
			fmt.Fprintf(b, "- `%s` — %s\n", f, rep.FileFailures[f])
		}
		b.WriteString("\n")
	}
	if len(rep.JavaFailures) > 0 {
		b.WriteString("### 無法解碼的 Java 檔\n\n")
		for _, f := range sortedKeys(rep.JavaFailures) {
			fmt.Fprintf(b, "- `%s` — %s\n", f, rep.JavaFailures[f])
		}
		b.WriteString("\n")
	}
	if len(rep.DuplicateFiles) > 0 {
		b.WriteString("### 同一支程序有多個原始檔\n\n")
		b.WriteString("比對用的是檔名排序在前的那一份（左邊）。上面標了 ⚠ 的列，" +
			"差異是對這一份算的，不代表另一份也不同。\n\n")
		for _, d := range rep.DuplicateFiles {
			fmt.Fprintf(b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	if len(rep.DuplicateDBNames) > 0 {
		b.WriteString("### 同名程序存在於多個 schema\n\n")
		for _, d := range rep.DuplicateDBNames {
			fmt.Fprintf(b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
}

// sites renders up to limit locations, marking the weaker evidence so a reader
// never mistakes a mention for a call.
func sites(list []javascan.Site, limit int) string {
	if len(list) == 0 {
		return "—"
	}
	var parts []string
	for i, s := range list {
		if i == limit {
			parts = append(parts, fmt.Sprintf("…等 %d 處", len(list)))
			break
		}
		mark := ""
		if s.Kind == javascan.KindMention {
			mark = "（僅提及）"
		}
		parts = append(parts, fmt.Sprintf("`%s:%d`%s", s.File, s.Line, mark))
	}
	return strings.Join(parts, "<br>")
}

// filePath renders the script path, flagging a name that several scripts
// define.
//
// HRM keeps 18 of these — dated variants such as sp_SRB0400_1011210.sql
// sitting beside sp_SRB0400.sql. The diff on such a row is against whichever
// file sorted first, which is deterministic but arbitrary, and four of the ten
// differing procedures are in this state. A reader who takes those diffs at
// face value would go and "fix" a file the database never came from.
func filePath(r Row) string {
	if r.FilePath == "" {
		return "—"
	}
	if n := len(r.OtherFiles); n > 0 {
		return fmt.Sprintf("`%s` ⚠ 另有 %d 個同名檔", r.FilePath, n)
	}
	return "`" + r.FilePath + "`"
}

// refCount summarises the Java evidence as counts.
func refCount(r Row) string {
	calls := len(r.Calls())
	mentions := len(r.CallSites) - calls
	switch {
	case calls == 0 && mentions == 0:
		return "—"
	case mentions == 0:
		return fmt.Sprintf("%d 呼叫", calls)
	case calls == 0:
		return fmt.Sprintf("%d 提及", mentions)
	}
	return fmt.Sprintf("%d 呼叫 / %d 提及", calls, mentions)
}

func clampDiff(diff string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n") + "\n"
	}
	kept := lines[:maxLines]
	return strings.Join(kept, "\n") +
		fmt.Sprintf("\n… 另有 %d 行，用 `hrm-sql-mcp sp diff` 看完整差異\n", len(lines)-maxLines)
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "**否**"
}

func targetLabel(k string) string {
	switch k {
	case "alias":
		return "目標別名"
	case "server":
		return "伺服器"
	case "database":
		return "資料庫"
	case "login":
		return "登入帳號"
	case "mode":
		return "存取模式"
	}
	return k
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
