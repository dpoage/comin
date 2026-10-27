package deployer

import (
	"testing"
	"time"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/nlewo/comin/internal/store"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestBasic(t *testing.T) {

	startedAt := time.Now()
	endedAt := startedAt.Add(10 * time.Second)
	deployment := &protobuf.Deployment{
		Uuid:         "uuid",
		Generation:   &protobuf.Generation{},
		StartedAt:    timestamppb.New(startedAt),
		EndedAt:      timestamppb.New(endedAt),
		ErrorMsg:     "",
		RestartComin: wrapperspb.Bool(false),
		ProfilePath:  "",
		Status:       store.StatusToString(store.Done),
		Operation:    "",
	}

	out, err := runPostDeploymentCommand("env", deployment)
	assert.NoError(t, err)
	assert.Contains(t, out, "COMIN_GIT_SHA=")
}

// C6: the post-deployment command sees COMIN_OPERATION equal to the
// deployment's operation. Mutant: delete the env line.
func TestCominOperationEnvVar(t *testing.T) {
	deployment := &protobuf.Deployment{
		Uuid:       "uuid",
		Generation: &protobuf.Generation{},
		Status:     store.StatusToString(store.Done),
		Operation:  "test",
	}

	out, err := runPostDeploymentCommand("env", deployment)
	assert.NoError(t, err)
	assert.Contains(t, out, "COMIN_OPERATION=test\n")
}

// A pre-fork deployment never had Operation set explicitly to "" on this
// path in practice, but the fallback still guarantees consumers never see
// an empty COMIN_OPERATION.
func TestCominOperationEnvVarFallsBackToSwitch(t *testing.T) {
	deployment := &protobuf.Deployment{
		Uuid:       "uuid",
		Generation: &protobuf.Generation{},
		Status:     store.StatusToString(store.Done),
		Operation:  "",
	}

	out, err := runPostDeploymentCommand("env", deployment)
	assert.NoError(t, err)
	assert.Contains(t, out, "COMIN_OPERATION=switch\n")
}
