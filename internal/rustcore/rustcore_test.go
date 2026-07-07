package rustcore

import (
	"context"
	"os"
	"sync"
	"testing"
)

func TestPlanMediaPathWithHelper(t *testing.T) {
	if os.Getenv(helperPathEnv) == "" {
		t.Skipf("set %s to test the Rust helper bridge", helperPathEnv)
	}
	helperState.once = sync.Once{}
	helperState.path = ""
	helperState.err = nil

	plan, err := PlanMediaPath(context.Background(), "downloads", true, MediaInfo{
		MessageID: 42,
		TDFileID:  7,
		MediaType: "photo",
		FileName:  "../a:b.jpg",
		FileSize:  100,
		MimeType:  "image/jpeg",
		ChatID:    123,
		TaskID:    "task-1",
	})
	if err != nil {
		t.Fatalf("PlanMediaPath() error = %v", err)
	}

	if plan.Directory != "downloads/chat_123/photo" {
		t.Fatalf("Directory = %q, want %q", plan.Directory, "downloads/chat_123/photo")
	}
	if plan.FileName != "__a_b.jpg" {
		t.Fatalf("FileName = %q, want %q", plan.FileName, "__a_b.jpg")
	}
	if plan.FilePath != "downloads/chat_123/photo/__a_b.jpg" {
		t.Fatalf("FilePath = %q, want %q", plan.FilePath, "downloads/chat_123/photo/__a_b.jpg")
	}
}
