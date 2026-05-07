#include <gtest/gtest.h>

#include <cstdio>
#include <filesystem>
#include <fstream>

#include "checkpoint.h"
#include "wordcount.h"

namespace fs = std::filesystem;
using jobworker::wordcount::counts_hash;
using jobworker::wordcount::read_state;
using jobworker::wordcount::run;
using jobworker::wordcount::WordCountEvent;
using jobworker::wordcount::WordCountState;
using jobworker::wordcount::write_state;

namespace {
std::string tmp_path(const std::string& tag) {
  auto p = fs::temp_directory_path() / ("wc_" + tag + "_" + std::to_string(::getpid()));
  return p.string();
}
}  // namespace

TEST(WordCount, DeterministicAcrossCadences) {
  WordCountState a;
  a.seed = 5;
  a.words_total = 200;
  std::uint64_t a_h = 0;
  run(a, 10, [&](const WordCountEvent& ev) { a_h = ev.found; });

  WordCountState b;
  b.seed = 5;
  b.words_total = 200;
  std::uint64_t b_h = 0;
  run(b, 47, [&](const WordCountEvent& ev) { b_h = ev.found; });

  EXPECT_EQ(a_h, b_h);
}

TEST(WordCount, DifferentSeedsDiverge) {
  WordCountState a;
  a.seed = 1;
  a.words_total = 200;
  std::uint64_t a_h = 0;
  run(a, 50, [&](const WordCountEvent& ev) { a_h = ev.found; });
  WordCountState b;
  b.seed = 2;
  b.words_total = 200;
  std::uint64_t b_h = 0;
  run(b, 50, [&](const WordCountEvent& ev) { b_h = ev.found; });
  EXPECT_NE(a_h, b_h);
}

TEST(WordCount, TotalCountsEqualWordsTotal) {
  WordCountState s;
  s.seed = 9;
  s.words_total = 1000;
  run(s, 100, [&](const WordCountEvent&) {});
  std::uint64_t total = 0;
  for (const auto& kv : s.counts) total += kv.second;
  EXPECT_EQ(total, s.words_total);
}

TEST(WordCount, RoundtripPreservesCounts) {
  WordCountState s;
  s.seed = 11;
  s.words_total = 500;
  run(s, 50, [&](const WordCountEvent&) {});
  const std::string path = tmp_path("rt");
  write_state(path, s);
  WordCountState got = read_state(path);
  EXPECT_EQ(got.seed, s.seed);
  EXPECT_EQ(got.words_total, s.words_total);
  EXPECT_EQ(got.next_word, s.next_word);
  EXPECT_EQ(counts_hash(got), counts_hash(s));
  fs::remove(path);
}

TEST(WordCount, ResumeMatchesFreshHash) {
  WordCountState fresh;
  fresh.seed = 21;
  fresh.words_total = 800;
  std::uint64_t fresh_h = 0;
  run(fresh, 50, [&](const WordCountEvent& ev) { fresh_h = ev.found; });

  WordCountState part;
  part.seed = 21;
  part.words_total = 800;
  bool taken = false;
  WordCountState snap;
  run(part, 25, [&](const WordCountEvent& ev) {
    if (!taken && ev.next_word > 400) {
      snap = part;
      taken = true;
    }
  });
  ASSERT_TRUE(taken);
  std::uint64_t resumed_h = 0;
  run(snap, 50, [&](const WordCountEvent& ev) { resumed_h = ev.found; });
  EXPECT_EQ(fresh_h, resumed_h);
}

TEST(WordCount, CrcCorruptionRejected) {
  WordCountState s;
  s.seed = 3;
  s.words_total = 50;
  run(s, 5, [&](const WordCountEvent&) {});
  const std::string path = tmp_path("crc");
  write_state(path, s);

  std::fstream f(path, std::ios::in | std::ios::out | std::ios::binary);
  ASSERT_TRUE(f.good());
  f.seekp(20);
  char b;
  f.read(&b, 1);
  b ^= 0xAA;
  f.seekp(20);
  f.write(&b, 1);
  f.close();
  bool threw = false;
  try {
    read_state(path);
  } catch (const jobworker::CheckpointError&) {
    threw = true;
  }
  EXPECT_TRUE(threw);
  fs::remove(path);
}
