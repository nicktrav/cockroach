// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/raft/raftlogger"
	pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"
)

var isLocalMsg = [...]bool{
	pb.MsgHup:         true,
	pb.MsgBeat:        true,
	pb.MsgUnreachable: true,
	pb.MsgSnapStatus:  true,
}

var isResponseMsg = [...]bool{
	pb.MsgAppResp:           true,
	pb.MsgVoteResp:          true,
	pb.MsgHeartbeatResp:     true,
	pb.MsgUnreachable:       true,
	pb.MsgPreVoteResp:       true,
	pb.MsgFortifyLeaderResp: true,
}

// isMsgFromLeader contains message types that come from the leader of the
// message's term.
var isMsgFromLeader = [...]bool{
	pb.MsgApp: true,
	// TODO(nvanbenschoten): we can't consider MsgSnap to be from the leader of
	// Message.Term until we address #127348 and #127349.
	// pb.MsgSnap:            true,
	pb.MsgHeartbeat:       true,
	pb.MsgTimeoutNow:      true,
	pb.MsgFortifyLeader:   true,
	pb.MsgDeFortifyLeader: true,
}

// isMsgIndicatingLeader contains message types that indicate that there is a
// leader at the message's term, even if the message is not from the leader
// itself.
//
// TODO(nvanbenschoten): remove this when we address the TODO above.
var isMsgIndicatingLeader = [...]bool{
	pb.MsgApp:             true,
	pb.MsgSnap:            true,
	pb.MsgHeartbeat:       true,
	pb.MsgTimeoutNow:      true,
	pb.MsgFortifyLeader:   true,
	pb.MsgDeFortifyLeader: true,
}

func isMsgInArray(msgt pb.MessageType, arr []bool) bool {
	i := int(msgt)
	return i < len(arr) && arr[i]
}

func IsLocalMsg(msgt pb.MessageType) bool {
	return isMsgInArray(msgt, isLocalMsg[:])
}

func IsResponseMsg(msgt pb.MessageType) bool {
	return isMsgInArray(msgt, isResponseMsg[:])
}

func IsMsgFromLeader(msgt pb.MessageType) bool {
	return isMsgInArray(msgt, isMsgFromLeader[:])
}

func IsMsgIndicatingLeader(msgt pb.MessageType) bool {
	return isMsgInArray(msgt, isMsgIndicatingLeader[:])
}

// senderHasMsgTerm returns true if the message type is one that should have
// the sender's term.
func senderHasMsgTerm(m pb.Message) bool {
	switch {
	case m.Type == pb.MsgPreVote:
		// We send pre-vote requests with a term in our future.
		return false
	case m.Type == pb.MsgPreVoteResp && !m.Reject:
		// We send pre-vote requests with a term in our future. If the
		// pre-vote is granted, we will increment our term when we get a
		// quorum. If it is not, the term comes from the node that
		// rejected our vote so we should become a follower at the new
		// term.
		return false
	default:
		// All other messages are sent with the sender's term.
		return true
	}
}

// voteResponseType maps vote and prevote message types to their corresponding responses.
func voteRespMsgType(msgt pb.MessageType) pb.MessageType {
	switch msgt {
	case pb.MsgVote:
		return pb.MsgVoteResp
	case pb.MsgPreVote:
		return pb.MsgPreVoteResp
	default:
		panic(fmt.Sprintf("not a vote message: %s", msgt))
	}
}

func DescribeHardState(hs pb.HardState) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "Term:%d", hs.Term)
	if hs.Vote != 0 {
		fmt.Fprintf(&buf, " Vote:%d", hs.Vote)
	}
	fmt.Fprintf(&buf, " Commit:%d", hs.Commit)
	fmt.Fprintf(&buf, " Lead:%d", hs.Lead)
	fmt.Fprintf(&buf, " LeadEpoch:%d", hs.LeadEpoch)
	return buf.String()
}

func DescribeSoftState(ss SoftState) string {
	return fmt.Sprintf("State:%s", ss.RaftState)
}

func DescribeSnapshot(snap pb.Snapshot) string {
	m := snap.Metadata
	return fmt.Sprintf("Index:%d Term:%d ConfState:%s",
		m.Index, m.Term, m.ConfState.Describe())
}

func DescribeReady(rd Ready, f EntryFormatter) string {
	var buf strings.Builder
	if rd.SoftState != nil {
		fmt.Fprint(&buf, DescribeSoftState(*rd.SoftState))
		buf.WriteByte('\n')
	}
	if !IsEmptyHardState(rd.HardState) {
		fmt.Fprintf(&buf, "HardState %s", DescribeHardState(rd.HardState))
		buf.WriteByte('\n')
	}
	if len(rd.Entries) > 0 {
		buf.WriteString("Entries:\n")
		fmt.Fprint(&buf, DescribeEntries(rd.Entries, f))
	}
	if rd.Snapshot != nil {
		fmt.Fprintf(&buf, "Snapshot %s\n", DescribeSnapshot(*rd.Snapshot))
	}
	if !rd.Committed.Empty() {
		fmt.Fprintf(&buf, "Committed: %s\n", rd.Committed)
	}
	if len(rd.Messages) > 0 {
		buf.WriteString("Messages:\n")
		for _, msg := range rd.Messages {
			fmt.Fprint(&buf, DescribeMessage(msg, f))
			buf.WriteByte('\n')
		}
	}
	if len(rd.Responses) > 0 {
		buf.WriteString("OnSync:\n")
		for _, msg := range rd.Responses {
			fmt.Fprint(&buf, DescribeMessage(msg, f))
			buf.WriteByte('\n')
		}
	}
	if buf.Len() > 0 {
		return fmt.Sprintf("Ready:\n%s", buf.String())
	}
	return "<empty Ready>"
}

// EntryFormatter can be implemented by the application to provide human-readable formatting
// of entry data. Nil is a valid EntryFormatter and will use a default format.
type EntryFormatter func([]byte) string

var emptyEntryFormatter EntryFormatter = func([]byte) string { return "" }

// DescribeMessage returns a concise human-readable description of a
// Message for debugging.
func DescribeMessage(m pb.Message, f EntryFormatter) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s->%s %v Term:%d Log:%d/%d",
		describeTarget(m.From), describeTarget(m.To), m.Type, m.Term, m.LogTerm, m.Index)
	if m.Reject {
		fmt.Fprintf(&buf, " Rejected (Hint: %d)", m.RejectHint)
	}
	if m.Commit != 0 {
		fmt.Fprintf(&buf, " Commit:%d", m.Commit)
	}
	if m.LeadEpoch != 0 {
		fmt.Fprintf(&buf, " LeadEpoch:%d", m.LeadEpoch)
	}
	if ln := len(m.Entries); ln == 1 {
		fmt.Fprintf(&buf, " Entries:[%s]", DescribeEntry(m.Entries[0], f))
	} else if ln > 1 {
		fmt.Fprint(&buf, " Entries:[")
		for _, e := range m.Entries {
			fmt.Fprintf(&buf, "\n  ")
			buf.WriteString(DescribeEntry(e, f))
		}
		fmt.Fprintf(&buf, "\n]")
	}
	if s := m.Snapshot; s != nil {
		fmt.Fprintf(&buf, "\n  Snapshot: %s", DescribeSnapshot(*s))
	}
	return buf.String()
}

func DescribeTarget(id pb.PeerID) string {
	return describeTarget(id)
}

func describeTarget(id pb.PeerID) string {
	if id == None {
		return "None"
	}
	return fmt.Sprintf("%x", id)
}

// DescribeEntry returns a concise human-readable description of an
// Entry for debugging.
func DescribeEntry(e pb.Entry, f EntryFormatter) string {
	if f == nil {
		f = func(data []byte) string { return fmt.Sprintf("%q", data) }
	}

	formatConfChange := func(cc pb.ConfChangeI) string {
		// TODO(tbg): give the EntryFormatter a type argument so that it gets
		// a chance to expose the Context.
		return pb.ConfChangesToString(cc.AsV2().Changes)
	}

	var formatted string
	switch e.Type {
	case pb.EntryNormal:
		formatted = f(e.Data)
	case pb.EntryConfChange:
		var cc pb.ConfChange
		if err := cc.Unmarshal(e.Data); err != nil {
			formatted = err.Error()
		} else {
			formatted = formatConfChange(cc)
		}
	case pb.EntryConfChangeV2:
		var cc pb.ConfChangeV2
		if err := cc.Unmarshal(e.Data); err != nil {
			formatted = err.Error()
		} else {
			formatted = formatConfChange(cc)
		}
	}
	if formatted != "" {
		formatted = " " + formatted
	}
	return fmt.Sprintf("%d/%d %s%s", e.Term, e.Index, e.Type, formatted)
}

// DescribeEntries calls DescribeEntry for each Entry, adding a newline to
// each.
func DescribeEntries(ents []pb.Entry, f EntryFormatter) string {
	var buf bytes.Buffer
	for _, e := range ents {
		_, _ = buf.WriteString(DescribeEntry(e, f) + "\n")
	}
	return buf.String()
}

// entryEncodingSize represents the protocol buffer encoding size of one or more
// entries.
type entryEncodingSize uint64

func entsSize(ents []pb.Entry) entryEncodingSize {
	var size entryEncodingSize
	for _, ent := range ents {
		size += entryEncodingSize(ent.Size())
	}
	return size
}

// limitSize returns the longest prefix of the given entries slice, such that
// its total byte size does not exceed maxSize. Always returns a non-empty slice
// if the input is non-empty, so, as an exception, if the size of the first
// entry exceeds maxSize, a non-empty slice with just this entry is returned.
func limitSize(ents []pb.Entry, maxSize entryEncodingSize) []pb.Entry {
	if len(ents) == 0 {
		return ents
	}
	size := ents[0].Size()
	for limit := 1; limit < len(ents); limit++ {
		size += ents[limit].Size()
		if entryEncodingSize(size) > maxSize {
			return ents[:limit]
		}
	}
	return ents
}

// entryPayloadSize represents the size of one or more entries' payloads.
// Notably, it does not depend on its Index or Term. Entries with empty
// payloads, like those proposed after a leadership change, are considered
// to be zero size.
type entryPayloadSize uint64

// payloadSize is the size of the payload of the provided entry.
func payloadSize(e pb.Entry) entryPayloadSize {
	return entryPayloadSize(len(e.Data))
}

// payloadsSize is the size of the payloads of the provided entries.
func payloadsSize(ents []pb.Entry) entryPayloadSize {
	var s entryPayloadSize
	for _, e := range ents {
		s += payloadSize(e)
	}
	return s
}

func assertConfStatesEquivalent(l raftlogger.Logger, cs1, cs2 pb.ConfState) {
	err := cs1.Equivalent(cs2)
	if err == nil {
		return
	}
	l.Panic(err)
}

// assertTrue panics with the supplied message if the condition does not hold
// true.
func assertTrue(condition bool, msg string) {
	if !condition {
		panic(msg)
	}
}

// extend appends vals to the given dst slice. It differs from the standard
// slice append only in the way it allocates memory. If cap(dst) is not enough
// for appending the values, precisely size len(dst)+len(vals) is allocated.
//
// Use this instead of standard append in situations when this is the last
// append to dst, so there is no sense in allocating more than needed.
func extend(dst, vals []pb.Entry) []pb.Entry {
	need := len(dst) + len(vals)
	if need <= cap(dst) {
		return append(dst, vals...) // does not allocate
	}
	buf := make([]pb.Entry, need, need) // allocates precisely what's needed
	copy(buf, dst)
	copy(buf[len(dst):], vals)
	return buf
}
