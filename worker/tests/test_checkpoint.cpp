#include <gtest/gtest.h>

#include <cstdio>
#include <filesystem>
#include <fstream>

#include "checkpoint.h"
#include "compute.h"

namespace fs = std::filesystem;
using jobworker::CheckpointError;
using jobworker::read_state;
using jobworker::SieveState;
using jobworker::write_state;

namespace {

std::string tmp_path(const std::string& tag) {
  auto p = fs::temp_directory_path() / ("jw_" + tag + "_" + std::to_string(::getpid()));
  return p.string();
}

}  // namespace

TEST(Checkpoint, RoundtripPreservesComputeFields) {
  SieveState s;
  s.limit = 1000;
  s.next = 17;
  s.found = 6;
  s.epoch = 3;  // intentionally non-zero; epoch is NOT persisted
  s.seeded_two = true;
  s.recent = {2, 3, 5, 7, 11, 13};

  const std::string path = tmp_path("rt");
  write_state(path, s);

  SieveState got = read_state(path);
  EXPECT_EQ(got.limit, s.limit);
  EXPECT_EQ(got.next, s.next);
  EXPECT_EQ(got.found, s.found);
  // Epoch is deliberately reset on read; the determinism contract excludes it.
  EXPECT_EQ(got.epoch, 0u);
  EXPECT_EQ(got.seeded_two, s.seeded_two);
  EXPECT_EQ(std::vector<std::uint64_t>(got.recent.begin(), got.recent.end()),
            std::vector<std::uint64_t>(s.recent.begin(), s.recent.end()));

  fs::remove(path);
}

TEST(Checkpoint, CorruptedTailIsRejected) {
  SieveState s;
  s.limit = 50;
  s.next = 7;
  s.found = 4;
  s.epoch = 1;
  s.seeded_two = true;
  s.recent = {2, 3, 5, 7};

  const std::string path = tmp_path("corrupt");
  write_state(path, s);

  // Flip a byte in the middle of the file.
  std::fstream f(path, std::ios::in | std::ios::out | std::ios::binary);
  ASSERT_TRUE(f.good());
  f.seekp(16);
  char buf;
  f.read(&buf, 1);
  buf = static_cast<char>(buf ^ 0xFF);
  f.seekp(16);
  f.write(&buf, 1);
  f.close();

  bool threw = false;
  try {
    read_state(path);
  } catch (const CheckpointError& e) {
    threw = true;
    EXPECT_EQ(e.kind, CheckpointError::Kind::kBadCrc);
  }
  EXPECT_TRUE(threw);

  fs::remove(path);
}

TEST(Checkpoint, TruncatedFileIsRejected) {
  SieveState s;
  s.limit = 50;
  s.next = 7;
  s.found = 4;
  s.epoch = 1;
  s.seeded_two = true;
  const std::string path = tmp_path("trunc");
  write_state(path, s);

  // Truncate to half its size.
  const auto sz = fs::file_size(path);
  std::filesystem::resize_file(path, sz / 2);

  bool threw = false;
  try {
    read_state(path);
  } catch (const CheckpointError&) {
    threw = true;
  }
  EXPECT_TRUE(threw);

  fs::remove(path);
}

TEST(Checkpoint, MissingFileIsRejected) {
  bool threw = false;
  try {
    read_state("/no/such/path/jw_should_not_exist");
  } catch (const CheckpointError& e) {
    threw = true;
    EXPECT_EQ(e.kind, CheckpointError::Kind::kOpenFailed);
  }
  EXPECT_TRUE(threw);
}
