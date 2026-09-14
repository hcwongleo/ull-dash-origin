package server

import (
	"os"
	"testing"
)

// CloudFormation refuses a template body over 51,200 bytes when it is passed
// inline, and inline is what `aws cloudformation deploy --template-file` does.
// Console upload goes via S3 and allows 1 MB, so the breakage only shows up on the
// automated path - the one least likely to be exercised by hand before a release.
//
// origin-stack.yaml carries a comment saying to stay under the limit. The comment
// did not work: it was breached while editing the template on 2026-09-14, and only
// caught because ValidateTemplate happened to be run. Hence a test.
func TestTemplateFitsCloudFormationInlineLimit(t *testing.T) {
	const limit = 51200

	info, err := os.Stat("../origin-stack.yaml")
	if err != nil {
		t.Fatalf("stat origin-stack.yaml: %v", err)
	}

	if size := info.Size(); size > limit {
		t.Errorf("origin-stack.yaml is %d bytes, %d over CloudFormation's %d-byte inline "+
			"limit: `aws cloudformation deploy --template-file` would refuse it. Move "+
			"rationale into docs/ rather than trimming what the template needs to say.",
			size, size-limit, limit)
	} else {
		t.Logf("origin-stack.yaml is %d bytes, %d to spare", size, limit-size)
	}
}
