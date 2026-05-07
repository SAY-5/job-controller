#include <gtest/gtest.h>

#include <cstdio>
#include <filesystem>
#include <fstream>

#include "checkpoint.h"
#include "matmul.h"

namespace fs = std::filesystem;
using jobworker::matmul::MatMulEvent;
using jobworker::matmul::MatMulState;
using jobworker::matmul::read_state;
using jobworker::matmul::run;
using jobworker::matmul::write_state;

namespace {
std::string tmp_path(const std::string& tag) {
  auto p = fs::temp_directory_path() / ("mm_" + tag + "_" + std::to_string(::getpid()));
  return p.string();
}
}  // namespace

TEST(MatMul, FreshRunFingerprintIsDeterministic) {
  MatMulState a;
  a.n = 8;
  a.seed = 42;
  std::uint64_t a_final = 0;
  run(a, 5, [&](const MatMulEvent& ev) { a_final = ev.found; });

  MatMulState b;
  b.n = 8;
  b.seed = 42;
  std::uint64_t b_final = 0;
  run(b, 11, [&](const MatMulEvent& ev) { b_final = ev.found; });

  EXPECT_EQ(a_final, b_final) << "fingerprint must not depend on checkpoint cadence";
}

TEST(MatMul, DifferentSeedsDiverge) {
  MatMulState a;
  a.n = 6;
  a.seed = 1;
  std::uint64_t a_final = 0;
  run(a, 4, [&](const MatMulEvent& ev) { a_final = ev.found; });
  MatMulState b;
  b.n = 6;
  b.seed = 2;
  std::uint64_t b_final = 0;
  run(b, 4, [&](const MatMulEvent& ev) { b_final = ev.found; });
  EXPECT_NE(a_final, b_final);
}

TEST(MatMul, RoundtripPreservesState) {
  MatMulState s;
  s.n = 12;
  s.seed = 7;
  s.next_cell = 13;
  s.fingerprint = 0xBADC0FFEE0DDF00DULL;
  const std::string path = tmp_path("rt");
  write_state(path, s);
  MatMulState got = read_state(path);
  EXPECT_EQ(got.n, s.n);
  EXPECT_EQ(got.seed, s.seed);
  EXPECT_EQ(got.next_cell, s.next_cell);
  EXPECT_EQ(got.fingerprint, s.fingerprint);
  fs::remove(path);
}

TEST(MatMul, ResumeFromMidProducesSameFingerprint) {
  MatMulState fresh;
  fresh.n = 10;
  fresh.seed = 99;
  std::uint64_t fresh_final = 0;
  run(fresh, 3, [&](const MatMulEvent& ev) { fresh_final = ev.found; });

  // Run halfway, snapshot, resume.
  MatMulState part;
  part.n = 10;
  part.seed = 99;
  bool taken = false;
  MatMulState snap;
  run(part, 1, [&](const MatMulEvent& ev) {
    if (!taken && ev.next_cell > (10 * 10) / 2) {
      snap = part;
      taken = true;
    }
  });
  ASSERT_TRUE(taken);
  std::uint64_t resumed_final = 0;
  run(snap, 5, [&](const MatMulEvent& ev) { resumed_final = ev.found; });
  EXPECT_EQ(fresh_final, resumed_final);
}

TEST(MatMul, CrcCorruptionRejected) {
  MatMulState s;
  s.n = 4;
  s.seed = 1;
  s.next_cell = 0;
  s.fingerprint = 0;
  const std::string path = tmp_path("crc");
  write_state(path, s);
  std::fstream f(path, std::ios::in | std::ios::out | std::ios::binary);
  ASSERT_TRUE(f.good());
  f.seekp(8);
  char b;
  f.read(&b, 1);
  b ^= 0x42;
  f.seekp(8);
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
