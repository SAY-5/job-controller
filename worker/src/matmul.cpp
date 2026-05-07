#include "matmul.h"

#include <fcntl.h>
#include <unistd.h>

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <stdexcept>
#include <vector>

#include "checkpoint.h"  // for CheckpointError

namespace jobworker::matmul {

namespace {

// Splitmix64 keeps the per-cell value generation tight and deterministic.
std::uint64_t splitmix(std::uint64_t& s) {
  s += 0x9E3779B97F4A7C15ULL;
  std::uint64_t z = s;
  z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
  z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
  return z ^ (z >> 31);
}

// Cell A[i,j] is splitmix(seed * 31 + (i*n + j) * 7 + 1).
std::uint64_t a_cell(std::uint64_t seed, std::uint64_t n, std::uint64_t i, std::uint64_t j) {
  std::uint64_t s = seed * 31ULL + (i * n + j) * 7ULL + 1ULL;
  return splitmix(s);
}

// Cell B[i,j] uses a different perturbation so AxB is not symmetric.
std::uint64_t b_cell(std::uint64_t seed, std::uint64_t n, std::uint64_t i, std::uint64_t j) {
  std::uint64_t s = seed * 17ULL + (i * n + j) * 11ULL + 2ULL;
  return splitmix(s);
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

std::uint32_t crc32_ieee(const std::uint8_t* data, std::size_t n) {
  static std::uint32_t table[256];
  static bool init = false;
  if (!init) {
    for (std::uint32_t i = 0; i < 256; ++i) {
      std::uint32_t c = i;
      for (int j = 0; j < 8; ++j) c = (c & 1) ? (0xEDB88320U ^ (c >> 1)) : (c >> 1);
      table[i] = c;
    }
    init = true;
  }
  std::uint32_t c = 0xFFFFFFFFU;
  for (std::size_t i = 0; i < n; ++i) c = table[(c ^ data[i]) & 0xFF] ^ (c >> 8);
  return c ^ 0xFFFFFFFFU;
}

}  // namespace

void run(MatMulState& state, std::uint64_t checkpoint_every_cells,
         const MatMulCallback& on_checkpoint) {
  if (checkpoint_every_cells == 0) checkpoint_every_cells = 1;
  const std::uint64_t total = state.n * state.n;
  std::uint64_t since_last = 0;
  while (state.next_cell < total) {
    const std::uint64_t i = state.next_cell / state.n;
    const std::uint64_t j = state.next_cell % state.n;
    std::uint64_t cell = 0;
    for (std::uint64_t k = 0; k < state.n; ++k) {
      cell += a_cell(state.seed, state.n, i, k) * b_cell(state.seed, state.n, k, j);
    }
    state.fingerprint += cell;
    state.next_cell += 1;
    since_last += 1;
    if (since_last >= checkpoint_every_cells) {
      state.epoch += 1;
      MatMulEvent ev;
      ev.epoch = state.epoch;
      ev.progress =
          total == 0 ? 0.0 : static_cast<double>(state.next_cell) / static_cast<double>(total);
      ev.found = state.fingerprint;
      ev.next_cell = state.next_cell;
      on_checkpoint(ev);
      since_last = 0;
    }
  }
  // Final emission.
  state.epoch += 1;
  MatMulEvent ev;
  ev.epoch = state.epoch;
  ev.progress = 1.0;
  ev.found = state.fingerprint;
  ev.next_cell = state.next_cell;
  on_checkpoint(ev);
}

void write_state(const std::string& path, const MatMulState& state) {
  // Layout: magic | version | n | seed | next_cell | fingerprint | crc
  std::vector<std::uint8_t> buf;
  put_u32(buf, kMagic);
  put_u32(buf, kVersion);
  put_u64(buf, state.n);
  put_u64(buf, state.seed);
  put_u64(buf, state.next_cell);
  put_u64(buf, state.fingerprint);
  std::uint32_t c = crc32_ieee(buf.data(), buf.size());
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

MatMulState read_state(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw CheckpointError(CheckpointError::Kind::kOpenFailed,
                          std::string("open ") + path + " for read");
  }
  std::vector<std::uint8_t> all((std::istreambuf_iterator<char>(in)),
                                std::istreambuf_iterator<char>());
  // header: 4 + 4 + 4*8 + 4 = 44 bytes minimum
  if (all.size() < 44) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "matmul: too short");
  }
  const std::size_t crc_off = all.size() - 4;
  const std::uint32_t expected = get_u32(all.data() + crc_off);
  const std::uint32_t actual = crc32_ieee(all.data(), crc_off);
  if (expected != actual) {
    throw CheckpointError(CheckpointError::Kind::kBadCrc, "matmul: crc");
  }
  const std::uint8_t* p = all.data();
  if (get_u32(p) != kMagic)
    throw CheckpointError(CheckpointError::Kind::kBadMagic, "matmul: magic");
  p += 4;
  if (get_u32(p) != kVersion)
    throw CheckpointError(CheckpointError::Kind::kBadVersion, "matmul: version");
  p += 4;
  MatMulState s;
  s.n = get_u64(p);
  p += 8;
  s.seed = get_u64(p);
  p += 8;
  s.next_cell = get_u64(p);
  p += 8;
  s.fingerprint = get_u64(p);
  return s;
}

}  // namespace jobworker::matmul
