#include "compute.h"

namespace jobworker {

bool is_prime_trial(std::uint64_t n) {
  if (n < 2) return false;
  if (n < 4) return true;
  if (n % 2 == 0) return false;
  for (std::uint64_t i = 3; i * i <= n; i += 2) {
    if (n % i == 0) return false;
  }
  return true;
}

namespace {
void emit(const SieveState& state, const CheckpointCallback& cb) {
  CheckpointEvent ev;
  ev.epoch = state.epoch;
  ev.found = state.found;
  ev.next = state.next;
  ev.progress =
      state.limit == 0
          ? 0.0
          : std::min(1.0, static_cast<double>(state.next) / static_cast<double>(state.limit));
  ev.recent.assign(state.recent.begin(), state.recent.end());
  cb(ev);
}
}  // namespace

void run_sieve(SieveState& state, std::uint64_t checkpoint_every_primes,
               const CheckpointCallback& on_checkpoint) {
  if (checkpoint_every_primes == 0) checkpoint_every_primes = 1;

  // Mark the "seeded" flag once the loop starts so a resume after epoch 0
  // doesn't restart from 2; the persisted state.next carries that for us.
  state.seeded_two = true;

  std::uint64_t since_last_checkpoint = 0;

  while (state.next <= state.limit) {
    const std::uint64_t candidate = state.next;
    bool prime_found = false;
    if (is_prime_trial(candidate)) {
      state.found += 1;
      state.recent.push_back(candidate);
      if (state.recent.size() > SieveState::kRecentCap) state.recent.pop_front();
      since_last_checkpoint += 1;
      prime_found = true;
    }
    // Advance state.next BEFORE the checkpoint emit so a resume from the
    // emitted state never re-counts the just-discovered prime.
    state.next = (candidate == 2) ? 3 : candidate + 2;

    if (prime_found && since_last_checkpoint >= checkpoint_every_primes) {
      state.epoch += 1;
      emit(state, on_checkpoint);
      since_last_checkpoint = 0;
    }
  }

  // Final emission so the controller always sees terminal state.
  state.epoch += 1;
  emit(state, on_checkpoint);
}

}  // namespace jobworker
