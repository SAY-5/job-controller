#pragma once

// wordcount: a deterministic, resumable word-count worker.
//
// The corpus is generated procedurally from `(seed, words_total)` using a
// Splitmix64 PRNG that picks words from a fixed dictionary. The job
// streams words in order, accumulating per-word counts; checkpoints fire
// every K words processed (default K=1000 per the layer-4 spec). The
// persisted state records the next word index plus a hash of the per-word
// counts so resumes can prove byte-identical convergence with a fresh run.

#include <cstdint>
#include <functional>
#include <string>
#include <unordered_map>

namespace jobworker::wordcount {

struct WordCountState {
  std::uint64_t seed = 0;                                   // PRNG seed -> deterministic corpus
  std::uint64_t words_total = 0;                            // corpus length
  std::uint64_t next_word = 0;                              // resume cursor
  std::uint64_t epoch = 0;                                  // not persisted
  std::unordered_map<std::uint32_t, std::uint64_t> counts;  // word_id -> count
};

struct WordCountEvent {
  std::uint64_t epoch;
  double progress;
  std::uint64_t found;  // = total counts hash; doubles as deterministic answer
  std::uint64_t next_word;
};

using WordCountCallback = std::function<void(const WordCountEvent&)>;

// run advances next_word to words_total, emitting a checkpoint every
// `checkpoint_every_words` words plus a final emission.
void run(WordCountState& state, std::uint64_t checkpoint_every_words,
         const WordCountCallback& on_checkpoint);

void write_state(const std::string& path, const WordCountState& state);
WordCountState read_state(const std::string& path);

// Deterministic hash over the per-word counts; used as the "found" value
// reported back to the controller and also as the byte-identity check
// for the chaos-recovery test.
std::uint64_t counts_hash(const WordCountState& state);

constexpr std::uint32_t kMagic = 0x57434E54U;  // "WCNT"
constexpr std::uint32_t kVersion = 1U;
constexpr std::uint32_t kDictSize = 64;  // small fixed dictionary

}  // namespace jobworker::wordcount
