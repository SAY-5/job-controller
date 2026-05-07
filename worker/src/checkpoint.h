#pragma once

#include <cstdint>
#include <stdexcept>
#include <string>

#include "compute.h"

namespace jobworker {

// Binary state file format (little-endian):
//   [u32 magic = 0x4A4F4243 ("JOBC")]
//   [u32 version = 2]
//   [u64 limit] [u64 next] [u64 found] [u8 seeded_two]
//   [u32 recent_count] [recent_count * u64 recent values]
//   [u32 crc32 of all preceding bytes]
//
// Notably absent: epoch. epoch is an in-process emission counter used
// for log correlation; it intentionally does not survive a resume, which
// keeps the determinism contract clean (a resumed run produces a
// byte-identical state file to a non-resumed run with the same args).
//
// Length-prefix framing for the recent ring keeps the resume contract
// self-describing. CRC32 (IEEE) detects truncation or corruption from
// crashes mid-write.
struct CheckpointError : std::runtime_error {
  enum class Kind {
    kOpenFailed,
    kTooShort,
    kBadMagic,
    kBadVersion,
    kBadCrc,
    kWriteFailed,
  };
  Kind kind;
  CheckpointError(Kind k, const std::string& msg) : std::runtime_error(msg), kind(k) {}
};

// Write state to path atomically: write to path+".tmp", fsync, rename.
void write_state(const std::string& path, const SieveState& state);

// Read state from path. Throws CheckpointError on any structural issue.
SieveState read_state(const std::string& path);

constexpr std::uint32_t kMagic = 0x4A4F4243U;
constexpr std::uint32_t kVersion = 2U;

}  // namespace jobworker
