// Implementation of components/store's shared write discipline — see
// include/chaski/fsutil.h for the contract.
#include "chaski/fsutil.h"

#include <dirent.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <cerrno>
#include <cstdlib>
#include <cstring>

namespace chaski::fsutil {
namespace {

// Temp files are dotfiles so ListNames skips them: a crash between create and
// rename must not leave something a directory scan mistakes for a letter.
constexpr char kTempPrefix[] = ".tmp.";

std::string DirOf(const std::string& path) {
  const std::string::size_type slash = path.find_last_of('/');
  if (slash == std::string::npos) return ".";
  if (slash == 0) return "/";
  return path.substr(0, slash);
}

std::string BaseOf(const std::string& path) {
  const std::string::size_type slash = path.find_last_of('/');
  return slash == std::string::npos ? path : path.substr(slash + 1);
}

bool WriteAll(int fd, const char* data, std::size_t len) {
  while (len > 0) {
    const ssize_t n = ::write(fd, data, len);
    if (n < 0) {
      if (errno == EINTR) continue;
      return false;
    }
    data += n;
    len -= static_cast<std::size_t>(n);
  }
  return true;
}

// SyncDir makes the rename itself durable. Without it the directory entry can
// still be in cache when the power goes, which loses the new file and leaves
// the old name pointing nowhere.
bool SyncDir(const std::string& dir) {
  const int fd = ::open(dir.c_str(), O_RDONLY);
  if (fd < 0) return false;
  const bool ok = ::fsync(fd) == 0;
  ::close(fd);
  return ok;
}

bool ParseI64(const std::string& s, std::int64_t& out) {
  if (s.empty()) return false;
  errno = 0;
  char* end = nullptr;
  const long long v = std::strtoll(s.c_str(), &end, 10);
  if (errno != 0 || end == s.c_str() || *end != '\0') return false;
  out = static_cast<std::int64_t>(v);
  return true;
}

}  // namespace

bool WriteAtomic(const std::string& path, const std::string& data) {
  const std::string dir = DirOf(path);
  const std::string tmp = Join(dir, kTempPrefix + BaseOf(path));

  const int fd = ::open(tmp.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0600);
  if (fd < 0) return false;
  if (!WriteAll(fd, data.data(), data.size()) || ::fsync(fd) != 0) {
    ::close(fd);
    ::unlink(tmp.c_str());
    return false;
  }
  if (::close(fd) != 0) {
    ::unlink(tmp.c_str());
    return false;
  }
  if (::rename(tmp.c_str(), path.c_str()) != 0) {
    ::unlink(tmp.c_str());
    return false;
  }
  return SyncDir(dir);
}

bool AppendLine(const std::string& path, const std::string& line) {
  const int fd = ::open(path.c_str(), O_WRONLY | O_CREAT | O_APPEND, 0600);
  if (fd < 0) return false;
  const std::string terminated = line + "\n";
  // One write call, so the kernel has the whole line at once and a torn append
  // is as short as the medium can make it. Readers drop the partial tail.
  const bool ok = WriteAll(fd, terminated.data(), terminated.size()) && ::fsync(fd) == 0;
  ::close(fd);
  return ok;
}

bool ReadAll(const std::string& path, std::string& out) {
  const int fd = ::open(path.c_str(), O_RDONLY);
  if (fd < 0) return false;
  std::string buf;
  char chunk[512];
  for (;;) {
    const ssize_t n = ::read(fd, chunk, sizeof(chunk));
    if (n < 0) {
      if (errno == EINTR) continue;
      ::close(fd);
      return false;
    }
    if (n == 0) break;
    buf.append(chunk, static_cast<std::size_t>(n));
  }
  ::close(fd);
  out.swap(buf);
  return true;
}

bool Remove(const std::string& path) {
  if (::unlink(path.c_str()) != 0) return errno == ENOENT;
  return SyncDir(DirOf(path));
}

bool Exists(const std::string& path) {
  struct stat st;
  return ::stat(path.c_str(), &st) == 0;
}

bool MkdirAll(const std::string& path) {
  if (path.empty()) return false;
  std::string built;
  std::string::size_type i = 0;
  while (i <= path.size()) {
    const std::string::size_type slash = path.find('/', i);
    const std::string::size_type end = slash == std::string::npos ? path.size() : slash;
    built.assign(path, 0, end);
    if (!built.empty() && ::mkdir(built.c_str(), 0700) != 0 && errno != EEXIST) return false;
    if (slash == std::string::npos) break;
    i = slash + 1;
  }
  struct stat st;
  return ::stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
}

bool ListNames(const std::string& dir, std::vector<std::string>& out) {
  DIR* d = ::opendir(dir.c_str());
  if (d == nullptr) return false;
  out.clear();
  while (const dirent* e = ::readdir(d)) {
    if (e->d_name[0] == '.') continue;
    out.emplace_back(e->d_name);
  }
  ::closedir(d);
  return true;
}

bool NameIsSafe(std::string_view name) {
  if (name.empty() || name.size() > 64) return false;
  if (name[0] == '.') return false;
  for (const char c : name) {
    const bool ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                    (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.';
    if (!ok) return false;
  }
  return true;
}

std::string Join(const std::string& dir, const std::string& name) {
  if (dir.empty()) return name;
  if (dir.back() == '/') return dir + name;
  return dir + "/" + name;
}

std::string EncodeRecord(const Record& r) {
  std::string out;
  for (const auto& [key, value] : r) {
    out += key;
    out += '=';
    for (const char c : value) {
      switch (c) {
        case '\\': out += "\\\\"; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        default: out += c;
      }
    }
    out += '\n';
  }
  return out;
}

Record DecodeRecord(std::string_view text) {
  Record r;
  for (const std::string& line : SplitCompleteLines(text)) {
    const std::string::size_type eq = line.find('=');
    if (eq == std::string::npos) continue;  // not a pair; ignore rather than fail
    std::string value;
    for (std::string::size_type i = eq + 1; i < line.size(); ++i) {
      if (line[i] != '\\' || i + 1 >= line.size()) {
        value += line[i];
        continue;
      }
      switch (line[++i]) {
        case 'n': value += '\n'; break;
        case 'r': value += '\r'; break;
        case '\\': value += '\\'; break;
        default: value += line[i];
      }
    }
    r.emplace_back(line.substr(0, eq), std::move(value));
  }
  return r;
}

const std::string* Find(const Record& r, std::string_view key) {
  for (const auto& [k, v] : r) {
    if (k == key) return &v;
  }
  return nullptr;
}

bool ReadInt(const Record& r, std::string_view key, int& out) {
  std::int64_t v = 0;
  if (!ReadI64(r, key, v)) return false;
  out = static_cast<int>(v);
  return true;
}

bool ReadI64(const Record& r, std::string_view key, std::int64_t& out) {
  const std::string* s = Find(r, key);
  return s != nullptr && ParseI64(*s, out);
}

bool ReadU64(const Record& r, std::string_view key, std::uint64_t& out) {
  std::int64_t v = 0;
  if (!ReadI64(r, key, v) || v < 0) return false;
  out = static_cast<std::uint64_t>(v);
  return true;
}

bool ReadBool(const Record& r, std::string_view key, bool& out) {
  const std::string* s = Find(r, key);
  if (s == nullptr) return false;
  if (*s == "1") {
    out = true;
    return true;
  }
  if (*s == "0") {
    out = false;
    return true;
  }
  return false;
}

std::vector<std::string> SplitCompleteLines(std::string_view text) {
  std::vector<std::string> lines;
  std::string::size_type start = 0;
  for (;;) {
    const std::string::size_type nl = text.find('\n', start);
    if (nl == std::string_view::npos) break;
    lines.emplace_back(text.substr(start, nl - start));
    start = nl + 1;
  }
  return lines;
}

}  // namespace chaski::fsutil
