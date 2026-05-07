#pragma once

// matmul: a deterministic, resumable matrix-multiply worker.
//
// We multiply A * B = C where A and B are NxN matrices generated from the
// (limit, seed) parameters. The job iterates over the C matrix in row-major
// order; checkpointing every K cells. The persisted state records the
// next cell index plus a running fingerprint (sum of computed cells mod
// 2^64) so resumes can detect divergence. Final fingerprint is the
// "found" value reported to the controller.
//
// The compute is intentionally O(N^3) so a moderate N (e.g. 200) takes
// long enough to meaningfully exercise the chaos-recovery path.

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

namespace jobworker::matmul {

struct MatMulState {
  std::uint64_t n = 0;            // matrix dimension
  std::uint64_t seed = 0;         // PRNG seed for A and B
  std::uint64_t next_cell = 0;    // next C cell to compute (row-major)
  std::uint64_t fingerprint = 0;  // sum of computed cells, mod 2^64
  std::uint64_t epoch = 0;        // in-process emission counter; not persisted
};

struct MatMulEvent {
  std::uint64_t epoch;
  double progress;
  std::uint64_t found;      // = fingerprint at emission time
  std::uint64_t next_cell;  // resume cursor
};

using MatMulCallback = std::function<void(const MatMulEvent&)>;

// run advances state until next_cell reaches n*n. Callback fires every
// `checkpoint_every_cells` newly-computed cells, plus a final emission.
void run(MatMulState& state, std::uint64_t checkpoint_every_cells,
         const MatMulCallback& on_checkpoint);

void write_state(const std::string& path, const MatMulState& state);
MatMulState read_state(const std::string& path);

constexpr std::uint32_t kMagic = 0x4D4D554CU;  // "MMUL"
constexpr std::uint32_t kVersion = 1U;

}  // namespace jobworker::matmul
