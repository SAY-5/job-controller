#include <gtest/gtest.h>

#include <vector>

#include "compute.h"

using jobworker::CheckpointEvent;
using jobworker::run_sieve;
using jobworker::SieveState;

namespace {

std::vector<std::uint64_t> all_primes_up_to(std::uint64_t limit) {
  // For test inputs that fit in the recent ring (limit producing <= kRecentCap
  // primes), the final emission contains every prime in order.
  SieveState s;
  s.limit = limit;
  std::vector<std::uint64_t> last;
  run_sieve(s, 1, [&](const CheckpointEvent& ev) { last = ev.recent; });
  return last;
}

}  // namespace

TEST(Compute, FindsKnownPrimesUpTo30) {
  auto primes = all_primes_up_to(30);
  std::vector<std::uint64_t> expected = {2, 3, 5, 7, 11, 13, 17, 19, 23, 29};
  EXPECT_EQ(primes, expected);
}

TEST(Compute, IsPrimeTrialBasic) {
  EXPECT_FALSE(jobworker::is_prime_trial(0));
  EXPECT_FALSE(jobworker::is_prime_trial(1));
  EXPECT_TRUE(jobworker::is_prime_trial(2));
  EXPECT_TRUE(jobworker::is_prime_trial(7919));
  EXPECT_FALSE(jobworker::is_prime_trial(7920));
}

TEST(Compute, ResumeMidRunMatchesFreshRun) {
  // Run 1: fresh run from 0 -> 200, count primes.
  SieveState fresh;
  fresh.limit = 200;
  std::uint64_t fresh_total = 0;
  run_sieve(fresh, 5, [&](const CheckpointEvent& ev) { fresh_total = ev.found; });

  // Run 2: run halfway, snapshot, build a new state from snapshot, finish.
  SieveState part;
  part.limit = 200;
  bool snapshot_taken = false;
  SieveState snapshot;
  run_sieve(part, 1, [&](const CheckpointEvent& ev) {
    if (!snapshot_taken && ev.next > 100) {
      snapshot = part;
      snapshot_taken = true;
    }
  });
  ASSERT_TRUE(snapshot_taken);

  std::uint64_t resumed_total = 0;
  run_sieve(snapshot, 5, [&](const CheckpointEvent& ev) { resumed_total = ev.found; });

  EXPECT_EQ(fresh_total, resumed_total);
  EXPECT_EQ(fresh.found, snapshot.found);
}

TEST(Compute, EpochIncrementsMonotonically) {
  SieveState s;
  s.limit = 100;
  std::uint64_t last_epoch = 0;
  bool ok = true;
  run_sieve(s, 3, [&](const CheckpointEvent& ev) {
    if (ev.epoch <= last_epoch && last_epoch != 0) ok = false;
    last_epoch = ev.epoch;
  });
  EXPECT_TRUE(ok);
  EXPECT_GT(last_epoch, 0u);
}
