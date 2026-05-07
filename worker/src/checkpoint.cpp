#include "checkpoint.h"

#include <fcntl.h>
#include <unistd.h>

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <vector>

namespace jobworker {

namespace {

// CRC32 (IEEE 802.3 polynomial, reflected). Standard table-driven
// implementation; computed in place to avoid pulling in zlib.
class Crc32 {
 public:
  Crc32() {
    for (std::uint32_t i = 0; i < 256; ++i) {
      std::uint32_t c = i;
      for (int j = 0; j < 8; ++j) {
        c = (c & 1) ? (0xEDB88320U ^ (c >> 1)) : (c >> 1);
      }
      table_[i] = c;
    }
  }
  std::uint32_t compute(const std::uint8_t* data, std::size_t n) const {
    std::uint32_t c = 0xFFFFFFFFU;
    for (std::size_t i = 0; i < n; ++i) {
      c = table_[(c ^ data[i]) & 0xFF] ^ (c >> 8);
    }
    return c ^ 0xFFFFFFFFU;
  }

 private:
  std::uint32_t table_[256];
};

const Crc32& crc() {
  static const Crc32 instance;
  return instance;
}

void put_u32(std::vector<std::uint8_t>& buf, std::uint32_t v) {
  for (int i = 0; i < 4; ++i) buf.push_back(static_cast<std::uint8_t>(v >> (8 * i)));
}
void put_u64(std::vector<std::uint8_t>& buf, std::uint64_t v) {
  for (int i = 0; i < 8; ++i) buf.push_back(static_cast<std::uint8_t>(v >> (8 * i)));
}
std::uint32_t get_u32(const std::uint8_t* p) {
  return static_cast<std::uint32_t>(p[0]) | (static_cast<std::uint32_t>(p[1]) << 8) |
         (static_cast<std::uint32_t>(p[2]) << 16) | (static_cast<std::uint32_t>(p[3]) << 24);
}
std::uint64_t get_u64(const std::uint8_t* p) {
  std::uint64_t v = 0;
  for (int i = 0; i < 8; ++i) v |= static_cast<std::uint64_t>(p[i]) << (8 * i);
  return v;
}

}  // namespace

void write_state(const std::string& path, const SieveState& state) {
  std::vector<std::uint8_t> buf;
  buf.reserve(64 + state.recent.size() * 8);
  put_u32(buf, kMagic);
  put_u32(buf, kVersion);
  put_u64(buf, state.limit);
  put_u64(buf, state.next);
  put_u64(buf, state.found);
  buf.push_back(state.seeded_two ? 1 : 0);
  put_u32(buf, static_cast<std::uint32_t>(state.recent.size()));
  for (auto p : state.recent) put_u64(buf, p);

  const std::uint32_t c = crc().compute(buf.data(), buf.size());
  put_u32(buf, c);

  const std::string tmp = path + ".tmp";
  int fd = ::open(tmp.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) {
    throw CheckpointError(CheckpointError::Kind::kOpenFailed,
                          std::string("open ") + tmp + ": " + std::strerror(errno));
  }
  std::size_t written = 0;
  while (written < buf.size()) {
    ssize_t n = ::write(fd, buf.data() + written, buf.size() - written);
    if (n < 0) {
      if (errno == EINTR) continue;
      ::close(fd);
      throw CheckpointError(CheckpointError::Kind::kWriteFailed,
                            std::string("write: ") + std::strerror(errno));
    }
    written += static_cast<std::size_t>(n);
  }
  if (::fsync(fd) != 0) {
    ::close(fd);
    throw CheckpointError(CheckpointError::Kind::kWriteFailed,
                          std::string("fsync: ") + std::strerror(errno));
  }
  ::close(fd);
  if (::rename(tmp.c_str(), path.c_str()) != 0) {
    throw CheckpointError(CheckpointError::Kind::kWriteFailed,
                          std::string("rename: ") + std::strerror(errno));
  }
}

SieveState read_state(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw CheckpointError(CheckpointError::Kind::kOpenFailed,
                          std::string("open ") + path + " for read");
  }
  std::vector<std::uint8_t> all((std::istreambuf_iterator<char>(in)),
                                std::istreambuf_iterator<char>());
  // header: 4 magic + 4 version + 3*u64 (limit/next/found) + 1 u8 + 4 u32 + 4 crc
  if (all.size() < 4 + 4 + 8 * 3 + 1 + 4 + 4) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "file too short");
  }

  const std::size_t crc_offset = all.size() - 4;
  const std::uint32_t expected = get_u32(all.data() + crc_offset);
  const std::uint32_t actual = crc().compute(all.data(), crc_offset);
  if (expected != actual) {
    throw CheckpointError(CheckpointError::Kind::kBadCrc, "crc mismatch");
  }

  const std::uint8_t* p = all.data();
  if (get_u32(p) != kMagic) {
    throw CheckpointError(CheckpointError::Kind::kBadMagic, "bad magic");
  }
  p += 4;
  if (get_u32(p) != kVersion) {
    throw CheckpointError(CheckpointError::Kind::kBadVersion, "bad version");
  }
  p += 4;

  SieveState s;
  s.limit = get_u64(p);
  p += 8;
  s.next = get_u64(p);
  p += 8;
  s.found = get_u64(p);
  p += 8;
  // epoch is not persisted; resumed run starts emitting from epoch 1.
  s.epoch = 0;
  s.seeded_two = (*p++ != 0);
  std::uint32_t rc = get_u32(p);
  p += 4;
  if (rc > SieveState::kRecentCap) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "recent ring too large");
  }
  for (std::uint32_t i = 0; i < rc; ++i) {
    s.recent.push_back(get_u64(p));
    p += 8;
  }
  return s;
}

}  // namespace jobworker
