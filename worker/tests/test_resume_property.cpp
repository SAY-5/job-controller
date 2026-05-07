// Hypothesis-style property test for the resume determinism contract.
//
// For N=100 random tuples (limit, seed, kill_position) we run a fresh
// pass to completion, run a second pass that "kills" itself at
// kill_position checkpoints in and snapshots state, then run a third pass
// resuming from that snapshot. The contract: the final SieveState (limit,
// next, found, recent, seeded_two) must match the fresh run byte-for-byte.
//
// `seed` is fed into a Mersenne Twister to derive the per-tuple parameters
// (checkpoint_every, sleep emulation, etc.) so the suite is fully
// deterministic given a fixed top-level seed but exercises 100 distinct
// configurations.

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <random>
#include <sstream>
#include <vector>

#include "checkpoint.h"
#include "compute.h"

using jobworker::CheckpointEvent;
using jobworker::run_sieve;
using jobworker::SieveState;

namespace {

// Run sieve to completion, return final state.
SieveState run_to_end(std::uint64_t limit, std::uint64_t every) {
  SieveState s;
  s.limit = limit;
  run_sieve(s, every, [](const CheckpointEvent&) {});
  return s;
}

// Run sieve, snapshot at the kill_index-th checkpoint emission, return
// the snapshot. If kill_index is past the total emissions, the final
// state is returned (which still validates the resume-from-end path).
SieveState run_until_kill(std::uint64_t limit, std::uint64_t every, int kill_index) {
  SieveState s;
  s.limit = limit;
  SieveState snapshot;
  bool taken = false;
  int seen = 0;
  run_sieve(s, every, [&](const CheckpointEvent&) {
    if (!taken) {
      if (seen == kill_index) {
        snapshot = s;
        taken = true;
      }
      seen += 1;
    }
  });
  if (!taken) snapshot = s;
  return snapshot;
}

// Resume from a partial state and run to completion.
SieveState resume_to_end(SieveState snapshot, std::uint64_t every) {
  run_sieve(snapshot, every, [](const CheckpointEvent&) {});
  return snapshot;
}

bool states_equivalent(const SieveState& a, const SieveState& b) {
  if (a.limit != b.limit) return false;
  if (a.next != b.next) return false;
  if (a.found != b.found) return false;
  if (a.seeded_two != b.seeded_two) return false;
  if (a.recent.size() != b.recent.size()) return false;
  return std::equal(a.recent.begin(), a.recent.end(), b.recent.begin());
}

std::string describe(const SieveState& s) {
  std::ostringstream o;
  o << "{limit=" << s.limit << " next=" << s.next << " found=" << s.found
    << " recent.size=" << s.recent.size() << "}";
  return o.str();
}

}  // namespace

class ResumeProperty : public ::testing::TestWithParam<int> {};

// 100 randomly-seeded tuples. Each Index from 0..99 derives its own
// (limit, every, kill_index) via a deterministic PRNG, so failures can be
// reproduced by re-running with the failing parameter index.
TEST_P(ResumeProperty, SnapshotResumeMatchesFreshRun) {
  const int idx = GetParam();
  std::mt19937_64 rng(static_cast<std::uint64_t>(idx) * 0x9E3779B97F4A7C15ULL + 1);

  // Limits in [200, 5000] to keep each pass under a few ms while still
  // producing >=10 checkpoints under typical every values.
  std::uniform_int_distribution<std::uint64_t> limit_dist(200, 5000);
  std::uniform_int_distribution<std::uint64_t> every_dist(1, 50);
  std::uniform_int_distribution<int> kill_dist(0, 30);

  const std::uint64_t limit = limit_dist(rng);
  const std::uint64_t every = every_dist(rng);
  const int kill = kill_dist(rng);

  const SieveState fresh = run_to_end(limit, every);
  const SieveState snap = run_until_kill(limit, every, kill);
  const SieveState resumed = resume_to_end(snap, every);

  ASSERT_TRUE(states_equivalent(fresh, resumed))
      << "tuple idx=" << idx << " limit=" << limit << " every=" << every << " kill=" << kill
      << " fresh=" << describe(fresh) << " resumed=" << describe(resumed);
}

INSTANTIATE_TEST_SUITE_P(N100, ResumeProperty, ::testing::Range(0, 100));

// Property: a resume from an arbitrarily *early* snapshot still converges.
// Specifically, snapshotting at checkpoint #0 (the very first emission) is
// the worst case -- the resume has the most work left to do.
TEST(ResumeProperty, EarliestSnapshotConverges) {
  const std::uint64_t limit = 1000;
  const std::uint64_t every = 1;
  const SieveState fresh = run_to_end(limit, every);
  const SieveState snap = run_until_kill(limit, every, 0);
  const SieveState resumed = resume_to_end(snap, every);
  EXPECT_TRUE(states_equivalent(fresh, resumed))
      << "fresh=" << describe(fresh) << " resumed=" << describe(resumed);
}

// Property: writing the snapshot to disk and reading it back must not
// disturb the resume contract. This couples the on-disk format to the
// in-memory state.
TEST(ResumeProperty, RoundtripThroughDiskPreservesResume) {
  const std::uint64_t limit = 500;
  const std::uint64_t every = 3;
  const SieveState fresh = run_to_end(limit, every);
  const SieveState mid = run_until_kill(limit, every, 5);

  const std::string path = std::string("/tmp/jw_resume_prop_") + std::to_string(::getpid());
  jobworker::write_state(path, mid);
  SieveState reloaded = jobworker::read_state(path);
  std::remove(path.c_str());

  const SieveState resumed = resume_to_end(reloaded, every);
  EXPECT_TRUE(states_equivalent(fresh, resumed))
      << "fresh=" << describe(fresh) << " resumed=" << describe(resumed);
}
