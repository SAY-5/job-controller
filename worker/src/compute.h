#pragma once

#include <cstdint>
#include <deque>
#include <functional>
#include <vector>

namespace jobworker {

// SieveState is the resumable state of a streaming prime sieve.
// next is the next candidate to test (always odd, >= 3, except initial state).
// recent holds the last N primes discovered, used by the determinism contract.
// found is the running count of primes discovered so far.
struct SieveState {
  std::uint64_t limit = 0;
  std::uint64_t next = 2;            // next candidate to test
  std::uint64_t found = 0;           // primes discovered so far
  std::uint64_t epoch = 0;           // incremented every checkpoint emit
  bool seeded_two = false;           // sentinel: loop has been entered at least once
  std::deque<std::uint64_t> recent;  // bounded ring of recent primes
  static constexpr std::size_t kRecentCap = 32;
};

// CheckpointEvent describes one checkpoint emission.
struct CheckpointEvent {
  std::uint64_t epoch;
  double progress;  // [0, 1]
  std::uint64_t found;
  std::uint64_t next;
  std::vector<std::uint64_t> recent;
};

using CheckpointCallback = std::function<void(const CheckpointEvent&)>;

// run_sieve advances state until next > limit. The callback fires every
// `checkpoint_every_primes` newly discovered primes (in addition to a final
// emission at end-of-job). It is deterministic: identical (state, limit,
// callback-side-effects) yield identical final state.
void run_sieve(SieveState& state, std::uint64_t checkpoint_every_primes,
               const CheckpointCallback& on_checkpoint);

// is_prime_trial is exposed for testing and is a simple O(sqrt n) check.
bool is_prime_trial(std::uint64_t n);

}  // namespace jobworker
