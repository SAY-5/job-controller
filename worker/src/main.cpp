// jobworker dispatches to one of three compute backends based on
// `--worker`: `primes` (default, the original sieve), `matmul`, or
// `wordcount`. All three share the same atomic-write/CRC-checked state
// file pattern; their on-disk layouts are documented in their respective
// headers (compute.h / matmul.h / wordcount.h).
//
// The controller is decoupled from this dispatch via the registry
// (cmd/controller reads worker_registry.yaml at startup and forwards
// the worker_name from the API request).

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <optional>
#include <sstream>
#include <string>
#include <string_view>
#include <thread>

#include "checkpoint.h"
#include "compute.h"
#include "matmul.h"
#include "wordcount.h"

namespace {

struct Args {
  std::string worker = "primes";
  std::string job_id;
  std::uint64_t limit = 100000;
  std::uint64_t checkpoint_every = 5000;
  std::uint64_t sleep_per_checkpoint_ms = 0;
  std::uint64_t seed = 1;
  std::string output_state;
  std::optional<std::string> resume_from;
};

void print_usage() {
  std::cerr << "usage: jobworker [--worker primes|matmul|wordcount] --job-id ID --limit N "
               "[--checkpoint-every K] [--seed S] [--output-state PATH] [--resume-from PATH]\n";
}

bool parse_u64(const std::string& s, std::uint64_t& out) {
  try {
    std::size_t pos = 0;
    out = std::stoull(s, &pos);
    return pos == s.size();
  } catch (...) {
    return false;
  }
}

bool parse_args(int argc, char** argv, Args& a) {
  for (int i = 1; i < argc; ++i) {
    std::string_view k = argv[i];
    auto need = [&](const char* name) -> const char* {
      if (i + 1 >= argc) {
        std::cerr << "missing value for " << name << "\n";
        return nullptr;
      }
      return argv[++i];
    };
    if (k == "--worker") {
      const char* v = need("--worker");
      if (!v) return false;
      a.worker = v;
    } else if (k == "--job-id") {
      const char* v = need("--job-id");
      if (!v) return false;
      a.job_id = v;
    } else if (k == "--limit") {
      const char* v = need("--limit");
      if (!v || !parse_u64(v, a.limit)) return false;
    } else if (k == "--checkpoint-every") {
      const char* v = need("--checkpoint-every");
      if (!v || !parse_u64(v, a.checkpoint_every)) return false;
    } else if (k == "--output-state") {
      const char* v = need("--output-state");
      if (!v) return false;
      a.output_state = v;
    } else if (k == "--resume-from") {
      const char* v = need("--resume-from");
      if (!v) return false;
      a.resume_from = v;
    } else if (k == "--sleep-per-checkpoint-ms") {
      const char* v = need("--sleep-per-checkpoint-ms");
      if (!v || !parse_u64(v, a.sleep_per_checkpoint_ms)) return false;
    } else if (k == "--seed") {
      const char* v = need("--seed");
      if (!v || !parse_u64(v, a.seed)) return false;
    } else if (k == "--help" || k == "-h") {
      print_usage();
      std::exit(0);
    } else {
      std::cerr << "unknown flag: " << k << "\n";
      return false;
    }
  }
  if (a.job_id.empty() || a.output_state.empty()) {
    print_usage();
    return false;
  }
  return true;
}

void emit_checkpoint(const std::string& job_id, std::uint64_t epoch, double progress,
                     std::uint64_t found, std::uint64_t cursor, const std::string& state_path) {
  std::ostringstream o;
  o << "{\"type\":\"checkpoint\",\"job_id\":\"" << job_id << "\",\"epoch\":" << epoch
    << ",\"progress\":" << progress << ",\"found\":" << found << ",\"next\":" << cursor
    << ",\"state_path\":\"" << state_path << "\"}";
  std::cout << o.str() << std::endl;
}

int run_primes(const Args& args) {
  jobworker::SieveState state;
  state.limit = args.limit;
  if (args.resume_from) {
    try {
      state = jobworker::read_state(*args.resume_from);
      if (state.limit != args.limit) {
        std::cerr << "resume limit mismatch: file=" << state.limit << " arg=" << args.limit << "\n";
        return 3;
      }
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "checkpoint error: " << e.what() << "\n";
      return 4;
    }
  }
  std::cout << "{\"type\":\"started\",\"job_id\":\"" << args.job_id << "\",\"worker\":\"primes\""
            << ",\"limit\":" << args.limit << ",\"resume_from_epoch\":" << state.epoch << "}"
            << std::endl;
  jobworker::run_sieve(state, args.checkpoint_every, [&](const jobworker::CheckpointEvent& ev) {
    try {
      jobworker::write_state(args.output_state, state);
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "checkpoint write failed: " << e.what() << "\n";
      std::exit(5);
    }
    emit_checkpoint(args.job_id, ev.epoch, ev.progress, ev.found, ev.next, args.output_state);
    if (args.sleep_per_checkpoint_ms > 0) {
      std::this_thread::sleep_for(std::chrono::milliseconds(args.sleep_per_checkpoint_ms));
    }
  });
  std::cout << "{\"type\":\"completed\",\"job_id\":\"" << args.job_id
            << "\",\"worker\":\"primes\",\"found\":" << state.found << ",\"epoch\":" << state.epoch
            << "}" << std::endl;
  return 0;
}

int run_matmul(const Args& args) {
  jobworker::matmul::MatMulState state;
  state.n = args.limit;
  state.seed = args.seed;
  if (args.resume_from) {
    try {
      state = jobworker::matmul::read_state(*args.resume_from);
      if (state.n != args.limit || state.seed != args.seed) {
        std::cerr << "matmul: resume parameters mismatch\n";
        return 3;
      }
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "matmul checkpoint error: " << e.what() << "\n";
      return 4;
    }
  }
  std::cout << "{\"type\":\"started\",\"job_id\":\"" << args.job_id << "\",\"worker\":\"matmul\""
            << ",\"n\":" << state.n << ",\"seed\":" << state.seed
            << ",\"resume_from_epoch\":" << state.epoch << "}" << std::endl;
  jobworker::matmul::run(
      state, args.checkpoint_every, [&](const jobworker::matmul::MatMulEvent& ev) {
        try {
          jobworker::matmul::write_state(args.output_state, state);
        } catch (const jobworker::CheckpointError& e) {
          std::cerr << "matmul write failed: " << e.what() << "\n";
          std::exit(5);
        }
        emit_checkpoint(args.job_id, ev.epoch, ev.progress, ev.found, ev.next_cell,
                        args.output_state);
        if (args.sleep_per_checkpoint_ms > 0) {
          std::this_thread::sleep_for(std::chrono::milliseconds(args.sleep_per_checkpoint_ms));
        }
      });
  std::cout << "{\"type\":\"completed\",\"job_id\":\"" << args.job_id
            << "\",\"worker\":\"matmul\",\"found\":" << state.fingerprint
            << ",\"epoch\":" << state.epoch << "}" << std::endl;
  return 0;
}

int run_wordcount(const Args& args) {
  jobworker::wordcount::WordCountState state;
  state.seed = args.seed;
  state.words_total = args.limit;
  if (args.resume_from) {
    try {
      state = jobworker::wordcount::read_state(*args.resume_from);
      if (state.words_total != args.limit || state.seed != args.seed) {
        std::cerr << "wordcount: resume parameters mismatch\n";
        return 3;
      }
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "wordcount checkpoint error: " << e.what() << "\n";
      return 4;
    }
  }
  std::cout << "{\"type\":\"started\",\"job_id\":\"" << args.job_id
            << "\",\"worker\":\"wordcount\",\"words_total\":" << state.words_total
            << ",\"seed\":" << state.seed << ",\"resume_from_epoch\":" << state.epoch << "}"
            << std::endl;
  jobworker::wordcount::run(
      state, args.checkpoint_every, [&](const jobworker::wordcount::WordCountEvent& ev) {
        try {
          jobworker::wordcount::write_state(args.output_state, state);
        } catch (const jobworker::CheckpointError& e) {
          std::cerr << "wordcount write failed: " << e.what() << "\n";
          std::exit(5);
        }
        emit_checkpoint(args.job_id, ev.epoch, ev.progress, ev.found, ev.next_word,
                        args.output_state);
        if (args.sleep_per_checkpoint_ms > 0) {
          std::this_thread::sleep_for(std::chrono::milliseconds(args.sleep_per_checkpoint_ms));
        }
      });
  std::cout << "{\"type\":\"completed\",\"job_id\":\"" << args.job_id
            << "\",\"worker\":\"wordcount\",\"found\":" << jobworker::wordcount::counts_hash(state)
            << ",\"epoch\":" << state.epoch << "}" << std::endl;
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  Args args;
  if (!parse_args(argc, argv, args)) return 2;
  if (args.worker == "primes") return run_primes(args);
  if (args.worker == "matmul") return run_matmul(args);
  if (args.worker == "wordcount") return run_wordcount(args);
  std::cerr << "unknown worker: " << args.worker << " (want primes|matmul|wordcount)\n";
  return 2;
}
