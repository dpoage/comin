package store

// C9: store.json stays loadable by be6025e's store.Load, which rejects
// unknown fields (protojson.UnmarshalOptions{} defaults DiscardUnknown to
// false). This is why every new comin state introduced by the override-lease
// feature (internal/leasestate) lives OUTSIDE store.json.
//
// This test pins be6025e's Store/Deployment/Generation wire schema as a
// protodesc-constructed descriptor (no protoc, no be6025e checkout needed at
// test time) and feeds the current code's Commit() output through a
// protojson.Unmarshal against a dynamicpb message built from that
// descriptor - exactly the compatibility check the old comin's store.Load
// performs. Mutant: add a field to the Store/Deployment/Generation proto
// messages; this test must then fail because the new field is "unknown" to
// the pinned descriptor.

import (
	"os"
	"testing"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/protobuf"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func strField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     &name,
		Number:   &number,
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		JsonName: &name,
	}
}

func msgField(name string, number int32, typeName string, repeated bool) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	return &descriptorpb.FieldDescriptorProto{
		Name:     &name,
		Number:   &number,
		Label:    label.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: &typeName,
		JsonName: &name,
	}
}

// be6025eStoreFile builds the be6025e (pre-round) wire schema for the
// Generation, Deployment and Store messages: exactly the fields present in
// internal/protobuf/services.proto's Generation/Deployment/Store messages
// before this round, which this round does not change (all new state lives
// in internal/leasestate instead).
func be6025eStoreFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	name := "be6025e_store_compat.proto"
	pkg := "be6025ecompat"
	syntax := "proto3"

	generation := &descriptorpb.DescriptorProto{
		Name: strPtr("Generation"),
		Field: []*descriptorpb.FieldDescriptorProto{
			strField("uuid", 1),
			strField("repository_path", 24),
			strField("repository_subdir", 25),
			strField("system_attr", 26),
			strField("hostname", 3),
			strField("selected_remote_url", 4),
			strField("selected_remote_name", 5),
			strField("selected_branch_name", 6),
			strField("selected_commit_id", 7),
			strField("selected_commit_msg", 8),
			msgField("selected_branch_is_testing", 9, ".google.protobuf.BoolValue", false),
			strField("main_commit_id", 10),
			strField("main_remote_name", 11),
			strField("main_branch_name", 12),
			strField("eval_status", 13),
			msgField("eval_started_at", 14, ".google.protobuf.Timestamp", false),
			msgField("eval_ended_at", 15, ".google.protobuf.Timestamp", false),
			strField("eval_err", 16),
			strField("out_path", 17),
			strField("drv_path", 18),
			strField("machine_id", 19),
			strField("build_status", 20),
			strField("build_reason", 27),
			msgField("build_started_at", 21, ".google.protobuf.Timestamp", false),
			msgField("build_ended_at", 22, ".google.protobuf.Timestamp", false),
			strField("build_err", 23),
		},
	}

	deployment := &descriptorpb.DescriptorProto{
		Name: strPtr("Deployment"),
		Field: []*descriptorpb.FieldDescriptorProto{
			strField("uuid", 1),
			strField("reason", 10),
			msgField("generation", 2, ".be6025ecompat.Generation", false),
			msgField("started_at", 3, ".google.protobuf.Timestamp", false),
			msgField("ended_at", 4, ".google.protobuf.Timestamp", false),
			strField("error_msg", 5),
			msgField("restart_comin", 6, ".google.protobuf.BoolValue", false),
			strField("profile_path", 7),
			strField("status", 8),
			strField("operation", 9),
		},
	}

	storeMsg := &descriptorpb.DescriptorProto{
		Name: strPtr("Store"),
		Field: []*descriptorpb.FieldDescriptorProto{
			msgField("deployments", 1, ".be6025ecompat.Deployment", true),
			msgField("generations", 2, ".be6025ecompat.Generation", true),
		},
	}

	fdProto := &descriptorpb.FileDescriptorProto{
		Name:       &name,
		Package:    &pkg,
		Syntax:     &syntax,
		Dependency: []string{"google/protobuf/timestamp.proto", "google/protobuf/wrappers.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			generation, deployment, storeMsg,
		},
	}

	fd, err := protodesc.NewFile(fdProto, protoregistry.GlobalFiles)
	assert.NoError(t, err)
	return fd
}

func strPtr(s string) *string { return &s }

func TestStoreJsonLoadableByBe6025e(t *testing.T) {
	fd := be6025eStoreFile(t)
	storeDesc := fd.Messages().ByName("Store")
	assert.NotNil(t, storeDesc)

	tmp := t.TempDir()
	bk := broker.New()
	bk.Start()
	s, err := New(bk, tmp+"/state.json", tmp+"/gcroots", 10, 10)
	assert.NoError(t, err)

	s.DeploymentInsert(&protobuf.Deployment{
		Uuid:      "dpl-1",
		Operation: "switch",
		Status:    StatusToString(Done),
		Generation: &protobuf.Generation{
			Uuid:             "gen-1",
			SelectedCommitId: "abc123",
			MainCommitId:     "abc123",
		},
	})
	assert.NoError(t, s.Commit())

	content, err := os.ReadFile(tmp + "/state.json")
	assert.NoError(t, err)

	legacy := dynamicpb.NewMessage(storeDesc)
	unmarshaler := protojson.UnmarshalOptions{} // default: DiscardUnknown is false
	err = unmarshaler.Unmarshal(content, legacy)
	assert.NoError(t, err, "store.json produced by the new comin must stay loadable by be6025e's store.Load")
}
