// TempRoot stands in for one device's letters partition for the length of a
// test. "Power loss" is simulated by destroying the store object and reopening
// it over the same directory: what survives is exactly what reached flash.
#pragma once

#include <unistd.h>

#include <cstdlib>
#include <string>
#include <vector>

#include "chaski/fsutil.h"

namespace chaski_test {

class TempRoot {
 public:
  TempRoot() {
    char tmpl[] = "/tmp/chaski-draft-XXXXXX";
    const char* d = ::mkdtemp(tmpl);
    path_ = d == nullptr ? "" : d;
  }
  ~TempRoot() {
    if (!path_.empty()) RemoveTree(path_);
  }
  TempRoot(const TempRoot&) = delete;
  TempRoot& operator=(const TempRoot&) = delete;

  const std::string& path() const { return path_; }

 private:
  static void RemoveTree(const std::string& dir) {
    std::vector<std::string> names;
    if (chaski::fsutil::ListNames(dir, names)) {
      for (const std::string& n : names) {
        const std::string child = chaski::fsutil::Join(dir, n);
        RemoveTree(child);
        ::unlink(child.c_str());
      }
    }
    ::rmdir(dir.c_str());
  }

  std::string path_;
};

}  // namespace chaski_test
