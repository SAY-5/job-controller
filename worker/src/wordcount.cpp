#include "wordcount.h"

#include <fcntl.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cstring>
#include <fstream>
#include <vector>

#include "checkpoint.h"  // CheckpointError

namespace jobworker::wordcount {

namespace {

std::uint64_t splitmix(std::uint64_t& s) {
  s += 0x9E3779B97F4A7C15ULL;
  std::uint64_t z = s;
  z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
  z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
  return z ^ (z >> 31);
}

// Word at position `idx` in the corpus is splitmix(seed + idx) % kDictSize.
std::uint32_t word_at(std::uint64_t seed, std::uint64_t idx) {
  std::uint64_t s = seed ^ (idx * 0xD1B54A32D192ED03ULL);
  return static_cast<std::uint32_t>(splitmix(s) % kDictSize);
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

std::uint64_t counts_hash(const WordCountState& state) {
  // Order-independent, byte-stable hash by sorting (word_id, count) pairs.
  std::vector<std::pair<std::uint32_t, std::uint64_t>> v;
  v.reserve(state.counts.size());
  for (const auto& kv : state.counts) v.emplace_back(kv.first, kv.second);
  std::sort(v.begin(), v.end());
  std::uint64_t h = 0xCBF29CE484222325ULL;
  for (const auto& kv : v) {
    h ^= kv.first;
    h *= 0x100000001B3ULL;
    h ^= kv.second;
    h *= 0x100000001B3ULL;
  }
  return h;
}

void run(WordCountState& state, std::uint64_t checkpoint_every_words,
         const WordCountCallback& on_checkpoint) {
  if (checkpoint_every_words == 0) checkpoint_every_words = 1;
  std::uint64_t since_last = 0;
  while (state.next_word < state.words_total) {
    const std::uint32_t w = word_at(state.seed, state.next_word);
    state.counts[w] += 1;
    state.next_word += 1;
    since_last += 1;
    if (since_last >= checkpoint_every_words) {
      state.epoch += 1;
      WordCountEvent ev;
      ev.epoch = state.epoch;
      ev.progress = state.words_total == 0 ? 0.0
                                           : static_cast<double>(state.next_word) /
                                                 static_cast<double>(state.words_total);
      ev.found = counts_hash(state);
      ev.next_word = state.next_word;
      on_checkpoint(ev);
      since_last = 0;
    }
  }
  state.epoch += 1;
  WordCountEvent ev;
  ev.epoch = state.epoch;
  ev.progress = 1.0;
  ev.found = counts_hash(state);
  ev.next_word = state.next_word;
  on_checkpoint(ev);
}

void write_state(const std::string& path, const WordCountState& state) {
  // Layout:
  //   magic | version | seed | words_total | next_word
  //   | u32 count_size | count_size * (u32 word_id, u64 count) | crc
  std::vector<std::uint8_t> buf;
  put_u32(buf, kMagic);
  put_u32(buf, kVersion);
  put_u64(buf, state.seed);
  put_u64(buf, state.words_total);
  put_u64(buf, state.next_word);

  // Sort counts so the byte representation is deterministic given the
  // same state.
  std::vector<std::pair<std::uint32_t, std::uint64_t>> sorted;
  sorted.reserve(state.counts.size());
  for (const auto& kv : state.counts) sorted.emplace_back(kv.first, kv.second);
  std::sort(sorted.begin(), sorted.end());
  put_u32(buf, static_cast<std::uint32_t>(sorted.size()));
  for (const auto& kv : sorted) {
    put_u32(buf, kv.first);
    put_u64(buf, kv.second);
  }
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

WordCountState read_state(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw CheckpointError(CheckpointError::Kind::kOpenFailed,
                          std::string("open ") + path + " for read");
  }
  std::vector<std::uint8_t> all((std::istreambuf_iterator<char>(in)),
                                std::istreambuf_iterator<char>());
  // minimum: 4 + 4 + 8 + 8 + 8 + 4 + 4 (zero counts) = 40
  if (all.size() < 40) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "wordcount: too short");
  }
  const std::size_t crc_off = all.size() - 4;
  const std::uint32_t expected = get_u32(all.data() + crc_off);
  const std::uint32_t actual = crc32_ieee(all.data(), crc_off);
  if (expected != actual) {
    throw CheckpointError(CheckpointError::Kind::kBadCrc, "wordcount: crc");
  }
  const std::uint8_t* p = all.data();
  if (get_u32(p) != kMagic)
    throw CheckpointError(CheckpointError::Kind::kBadMagic, "wordcount: magic");
  p += 4;
  if (get_u32(p) != kVersion)
    throw CheckpointError(CheckpointError::Kind::kBadVersion, "wordcount: version");
  p += 4;
  WordCountState s;
  s.seed = get_u64(p);
  p += 8;
  s.words_total = get_u64(p);
  p += 8;
  s.next_word = get_u64(p);
  p += 8;
  std::uint32_t count_size = get_u32(p);
  p += 4;
  // Bound the count size by kDictSize so a malicious file can't trigger
  // unbounded allocations.
  if (count_size > kDictSize) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "wordcount: counts overflow");
  }
  // Each count entry is 4+8 = 12 bytes; ensure we have that much body left.
  const std::size_t needed = static_cast<std::size_t>(count_size) * 12;
  if (static_cast<std::size_t>(crc_off) - (p - all.data()) != needed) {
    throw CheckpointError(CheckpointError::Kind::kTooShort, "wordcount: count body size mismatch");
  }
  for (std::uint32_t i = 0; i < count_size; ++i) {
    std::uint32_t wid = get_u32(p);
    p += 4;
    std::uint64_t cnt = get_u64(p);
    p += 8;
    s.counts[wid] = cnt;
  }
  return s;
}

}  // namespace jobworker::wordcount
