#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <optional>
#include <sstream>
#include <string>
#include <string_view>
#include <thread>

#include "checkpoint.h"
#include "compute.h"

namespace {

struct Args {
  std::string job_id;
  std::uint64_t limit = 100000;
  std::uint64_t checkpoint_every = 5000;
  std::uint64_t sleep_per_checkpoint_ms = 0;  // optional pacing for chaos tests
  std::string output_state;
  std::optional<std::string> resume_from;
};

void print_usage() {
  std::cerr << "usage: jobworker --job-id ID --limit N "
               "[--checkpoint-every K] [--output-state PATH] "
               "[--resume-from PATH]\n";
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
    if (k == "--job-id") {
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

// Emit a checkpoint event as a JSON line to stdout. Recent primes are
// serialized as a JSON array; the controller persists the latest payload.
void emit_event_line(const std::string& job_id, const jobworker::CheckpointEvent& ev,
                     const std::string& state_path) {
  std::ostringstream o;
  o << "{\"type\":\"checkpoint\",\"job_id\":\"" << job_id << "\",\"epoch\":" << ev.epoch
    << ",\"progress\":" << ev.progress << ",\"found\":" << ev.found << ",\"next\":" << ev.next
    << ",\"state_path\":\"" << state_path << "\",\"recent\":[";
  for (std::size_t i = 0; i < ev.recent.size(); ++i) {
    if (i) o << ",";
    o << ev.recent[i];
  }
  o << "]}";
  std::cout << o.str() << std::endl;  // flush so the controller sees it
}

}  // namespace

int main(int argc, char** argv) {
  Args args;
  if (!parse_args(argc, argv, args)) return 2;

  jobworker::SieveState state;
  state.limit = args.limit;

  if (args.resume_from) {
    try {
      state = jobworker::read_state(*args.resume_from);
      // Resume must respect the original limit; refuse if mismatched.
      if (state.limit != args.limit) {
        std::cerr << "resume limit mismatch: file=" << state.limit << " arg=" << args.limit << "\n";
        return 3;
      }
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "checkpoint error: " << e.what() << "\n";
      return 4;
    }
  }

  // Emit a starting line so the controller knows we're alive.
  std::cout << "{\"type\":\"started\",\"job_id\":\"" << args.job_id << "\",\"limit\":" << args.limit
            << ",\"resume_from_epoch\":" << state.epoch << "}" << std::endl;

  jobworker::run_sieve(state, args.checkpoint_every, [&](const jobworker::CheckpointEvent& ev) {
    try {
      jobworker::write_state(args.output_state, state);
    } catch (const jobworker::CheckpointError& e) {
      std::cerr << "checkpoint write failed: " << e.what() << "\n";
      std::exit(5);
    }
    emit_event_line(args.job_id, ev, args.output_state);
    if (args.sleep_per_checkpoint_ms > 0) {
      std::this_thread::sleep_for(std::chrono::milliseconds(args.sleep_per_checkpoint_ms));
    }
  });

  std::cout << "{\"type\":\"completed\",\"job_id\":\"" << args.job_id
            << "\",\"found\":" << state.found << ",\"epoch\":" << state.epoch << "}" << std::endl;
  return 0;
}
