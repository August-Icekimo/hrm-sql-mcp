package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
)

func cmdExecute(args []string) error {
	fs := flag.NewFlagSet("execute", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias（必須是政策標記可寫的）")
	commit := fs.Bool("commit", false, "真的寫入；預設是演練（跑完 rollback）")
	approvalID := fs.String("approval", "", "已核可的 approval id")
	reason := fs.String("reason", "", "為什麼要做這個變更，會顯示給核可的人")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}
	statement, err := readStatement(fs.Args())
	if err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	res, t, err := svc.Execute(context.Background(), *alias, statement, service.WriteOptions{
		Commit:     *commit,
		ApprovalID: *approvalID,
		Reason:     *reason,
	})
	if err != nil {
		return explainWriteError(err)
	}

	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s @ %s/%s 以 %s 身分\n", d["alias"], d["server"], d["database"], d["login"])
	printWriteResult(res)
	return nil
}

// explainWriteError keeps the approval handshake from reading as a failure.
//
// The first commit attempt always comes back needing approval. If that printed
// like an error, the natural next move would be to look for a way around it —
// which is precisely the wrong instinct to build.
func explainWriteError(err error) error {
	var need *service.ErrApprovalRequired
	if errors.As(err, &need) {
		fmt.Fprintf(os.Stderr, "\n需要核可，目前什麼都還沒執行。\n\n")
		fmt.Fprintf(os.Stderr, "  1. 另開一個終端機，執行：hrm-sql-mcp approve %s\n", need.ID)
		fmt.Fprintf(os.Stderr, "  2. 讀完語句後決定\n")
		fmt.Fprintf(os.Stderr, "  3. 回來用 --approval %s 加上完全相同的語句再跑一次\n\n", need.ID)
		fmt.Fprintf(os.Stderr, "待核可的請求放在 %s\n", need.Dir)
		return errors.New("approval required")
	}
	return err
}

func printWriteResult(res *service.WriteResult) {
	if res.ExecResult == nil {
		return
	}
	if res.Committed {
		fmt.Printf("已提交：%d 列受影響（%d ms）\n", res.RowsAffected, res.ElapsedMS)
	} else {
		fmt.Printf("演練（已 rollback）：會影響 %d 列（%d ms）\n", res.RowsAffected, res.ElapsedMS)
	}
	fmt.Printf("分類：%s\n", res.Label.Summary())
	if res.SnapshotPath != "" {
		fmt.Printf("舊定義已存到：%s\n", res.SnapshotPath)
	}
	for _, c := range res.Caveats {
		fmt.Fprintf(os.Stderr, "-- ⚠ %s\n", c)
	}
}

func cmdSPDeploy(args []string) error {
	fs := flag.NewFlagSet("sp deploy", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias（必須是政策標記可寫的）")
	name := fs.String("name", "", "程序名稱，用來在覆蓋前存下目前的定義")
	commit := fs.Bool("commit", false, "真的部署；預設是演練（編譯後 rollback）")
	approvalID := fs.String("approval", "", "已核可的 approval id")
	reason := fs.String("reason", "", "為什麼要部署，會顯示給核可的人")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("sp deploy 需要 --name：沒有名稱就無法在覆蓋前存下舊定義")
	}
	definition, err := readStatement(fs.Args())
	if err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	res, t, err := svc.SPDeploy(context.Background(), *alias, *name, definition, service.WriteOptions{
		Commit:     *commit,
		ApprovalID: *approvalID,
		Reason:     *reason,
	})
	if err != nil {
		return explainWriteError(err)
	}

	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s @ %s/%s 以 %s 身分\n", d["alias"], d["server"], d["database"], d["login"])
	printWriteResult(res)
	return nil
}
