package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The wire schema of the persisted messages (store.json) at 3e397db. A
// comin that rolls back to it rejects an unknown field and loads no
// deployment, so these messages must not change.
const persistedSchema = `Store.deployments=1 message repeated protobuf.Deployment
Store.generations=2 message repeated protobuf.Generation
Deployment.uuid=1 string optional 
Deployment.reason=10 string optional 
Deployment.generation=2 message optional protobuf.Generation
Deployment.started_at=3 message optional google.protobuf.Timestamp
Deployment.ended_at=4 message optional google.protobuf.Timestamp
Deployment.error_msg=5 string optional 
Deployment.restart_comin=6 message optional google.protobuf.BoolValue
Deployment.profile_path=7 string optional 
Deployment.status=8 string optional 
Deployment.operation=9 string optional 
Generation.uuid=1 string optional 
Generation.repository_path=24 string optional 
Generation.repository_subdir=25 string optional 
Generation.system_attr=26 string optional 
Generation.hostname=3 string optional 
Generation.selected_remote_url=4 string optional 
Generation.selected_remote_name=5 string optional 
Generation.selected_branch_name=6 string optional 
Generation.selected_commit_id=7 string optional 
Generation.selected_commit_msg=8 string optional 
Generation.selected_branch_is_testing=9 message optional google.protobuf.BoolValue
Generation.main_commit_id=10 string optional 
Generation.main_remote_name=11 string optional 
Generation.main_branch_name=12 string optional 
Generation.eval_status=13 string optional 
Generation.eval_started_at=14 message optional google.protobuf.Timestamp
Generation.eval_ended_at=15 message optional google.protobuf.Timestamp
Generation.eval_err=16 string optional 
Generation.out_path=17 string optional 
Generation.drv_path=18 string optional 
Generation.machine_id=19 string optional 
Generation.build_status=20 string optional 
Generation.build_reason=27 string optional 
Generation.build_started_at=21 message optional google.protobuf.Timestamp
Generation.build_ended_at=22 message optional google.protobuf.Timestamp
Generation.build_err=23 string optional `

func dumpSchema(m protoreflect.MessageDescriptor, seen map[string]bool, out *[]string) {
	if seen[string(m.FullName())] {
		return
	}
	seen[string(m.FullName())] = true
	fields := m.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		target := ""
		if f.Message() != nil {
			target = string(f.Message().FullName())
		}
		*out = append(*out, fmt.Sprintf("%s.%s=%d %s %s %s", m.Name(), f.Name(), f.Number(), f.Kind(), f.Cardinality(), target))
	}
	for i := range fields.Len() {
		f := fields.Get(i)
		if f.Message() != nil && f.Message().ParentFile().Path() == m.ParentFile().Path() {
			dumpSchema(f.Message(), seen, out)
		}
	}
}

func TestStoreFileSchemaIsThePersistedOneOf3e397db(t *testing.T) {
	var got []string
	dumpSchema((&protobuf.Store{}).ProtoReflect().Descriptor(), map[string]bool{}, &got)
	assert.Equal(t, strings.Split(persistedSchema, "\n"), got)
}
