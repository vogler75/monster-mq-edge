// TLV encoding of the MonsterMQ embedding contract (spec-winccoa-native.md
// section 3.1/3.2) and conversion between WinCC OA Variables and TLV values.
#ifndef MMQ_TLV_HXX
#define MMQ_TLV_HXX

#include <cstdint>
#include <cstring>
#include <string>
#include <functional>
#include <vector>

#include <DpElementType.hxx>
#include <Variable.hxx>

namespace mmq
{

enum Tag : uint8_t
{
  TagOp = 1, TagName = 2, TagQuery = 3, TagFlags = 4, TagValue = 5, TagRef = 6, TagError = 7,
  TagRow = 8, TagTypeName = 9, TagElemType = 10, TagSysName = 11, TagExists = 12, TagTime = 13, TagCount = 14
};

enum Kind : uint8_t
{
  KindNull = 0, KindBool = 1, KindInt = 2, KindUint = 3, KindFloat = 4, KindString = 5, KindTime = 6,
  KindBytes = 7, KindDyn = 8, KindLangText = 9, KindBit32 = 10
};

// Element types reported to Go: value kinds, 0 = structure node, 255 = unsupported.
const uint32_t ElemStruct = 0;
const uint32_t ElemUnsupported = 255;

enum Op : uint32_t
{
  OpResolve = 1, OpSysInfo = 2, OpQueryConnect = 3, OpQueryDisconnect = 4, OpDpConnect = 5,
  OpDpDisconnect = 6, OpDpSet = 7, OpDpGet = 8, OpDpNames = 9, OpDpCreate = 10, OpDpDelete = 11,
  OpTypeCheck = 12
};

const uint32_t FlagAnswer = 1u << 0;
const uint32_t FlagNoSource = 1u << 1;
const uint32_t FlagMore = 1u << 3;  // query answer continues in another event

class Writer
{
  public:
    void raw(uint8_t tag, const void *p, uint32_t n);
    void str(uint8_t tag, const std::string &s) { raw(tag, s.data(), (uint32_t)s.size()); }
    void str(uint8_t tag, const char *s) { raw(tag, s, (uint32_t)strlen(s)); }
    void u32(uint8_t tag, uint32_t v);
    void u64(uint8_t tag, uint64_t v);
    void boolean(uint8_t tag, bool v) { uint8_t b = v ? 1 : 0; raw(tag, &b, 1); }
    // Encodes a Variable as a TLV value field (kind + body).
    void value(uint8_t tag, const Variable *v);
    void nested(uint8_t tag, const Writer &inner) { raw(tag, inner.data(), inner.size()); }
    // Appends the fields of another writer as they are.
    void append(const Writer &other) { buf.insert(buf.end(), other.buf.begin(), other.buf.end()); }

    const uint8_t *data() const { return buf.empty() ? nullptr : buf.data(); }
    uint32_t size() const { return (uint32_t)buf.size(); }
    bool empty() const { return buf.empty(); }

  private:
    std::vector<uint8_t> buf;
};

struct Field
{
  uint8_t tag;
  const uint8_t *p;
  uint32_t n;
};

class Message
{
  public:
    // Returns false for a malformed message.
    bool parse(const uint8_t *p, uint32_t n);
    const Field *first(uint8_t tag) const;
    std::vector<const Field *> all(uint8_t tag) const;
    std::string str(uint8_t tag) const;
    bool u32(uint8_t tag, uint32_t &out) const;
    bool u64(uint8_t tag, uint64_t &out) const;
    const std::vector<Field> &fields() const { return list; }

  private:
    std::vector<Field> list;
};

std::string fieldString(const Field *f);

// Maps an OA element type to the kind number reported to Go.
uint32_t kindOfElement(DpElementType et);

// Creates a new Variable of the element's type from an encoded TLV value.
// Returns nullptr and sets err when the kinds do not match exactly.
Variable *decodeValue(const Field &f, DpElementType et, std::string &err);

// Encodes a query result table (DynVar of DynVar, header row first) as
// repeated TagRow fields. Returns false if v is not a table.
bool encodeTable(Writer &w, const Variable *v);

// Splits a query result table into row chunks of at most maxBytes, each
// starting with the header row, and calls sink(rows, last) per chunk.
// Returns false if v is not a table.
bool encodeTableChunks(const Variable *v, uint32_t maxBytes, const std::function<void(const Writer &, bool)> &sink);

}  // namespace mmq

#endif
