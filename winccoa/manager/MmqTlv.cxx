#include "MmqTlv.hxx"

#include <Manager.hxx>
#include <AnyTypeVar.hxx>
#include <Bit32Var.hxx>
#include <Bit64Var.hxx>
#include <BitVar.hxx>
#include <Blob.hxx>
#include <BlobVar.hxx>
#include <CharVar.hxx>
#include <DpIdentifierVar.hxx>
#include <DynVar.hxx>
#include <FloatVar.hxx>
#include <IntegerVar.hxx>
#include <LangText.hxx>
#include <LangTextVar.hxx>
#include <LongVar.hxx>
#include <TextVar.hxx>
#include <TimeVar.hxx>
#include <UIntegerVar.hxx>
#include <ULongVar.hxx>

namespace mmq
{

static void putLE(std::vector<uint8_t> &b, uint64_t v, int bytes)
{
  for (int i = 0; i < bytes; i++)
    b.push_back((uint8_t)(v >> (8 * i)));
}

static uint64_t getLE(const uint8_t *p, int bytes)
{
  uint64_t v = 0;
  for (int i = 0; i < bytes; i++)
    v |= (uint64_t)p[i] << (8 * i);
  return v;
}

void Writer::raw(uint8_t tag, const void *p, uint32_t n)
{
  buf.push_back(tag);
  putLE(buf, n, 4);
  const uint8_t *b = static_cast<const uint8_t *>(p);
  if (n)
    buf.insert(buf.end(), b, b + n);
}

void Writer::u32(uint8_t tag, uint32_t v)
{
  std::vector<uint8_t> b;
  putLE(b, v, 4);
  raw(tag, b.data(), 4);
}

void Writer::u64(uint8_t tag, uint64_t v)
{
  std::vector<uint8_t> b;
  putLE(b, v, 8);
  raw(tag, b.data(), 8);
}

static void encodeInto(std::vector<uint8_t> &out, const Variable *v);

void Writer::value(uint8_t tag, const Variable *v)
{
  std::vector<uint8_t> body;
  encodeInto(body, v);
  raw(tag, body.data(), (uint32_t)body.size());
}

static void encodeInto(std::vector<uint8_t> &out, const Variable *v)
{
  if (!v)
  {
    out.push_back(KindNull);
    return;
  }
  switch (v->isA())
  {
    case BIT_VAR:
      out.push_back(KindBool);
      out.push_back(static_cast<const BitVar *>(v)->getValue() ? 1 : 0);
      return;
    case INTEGER_VAR:
      out.push_back(KindInt);
      putLE(out, (uint64_t)(int64_t) static_cast<const IntegerVar *>(v)->getValue(), 8);
      return;
    case CHAR_VAR:
      out.push_back(KindInt);
      putLE(out, (uint64_t) static_cast<const CharVar *>(v)->getValue(), 8);
      return;
    case LONG_VAR:
      out.push_back(KindInt);
      putLE(out, (uint64_t) static_cast<const LongVar *>(v)->getValue(), 8);
      return;
    case UINTEGER_VAR:
      out.push_back(KindUint);
      putLE(out, (uint64_t) static_cast<const UIntegerVar *>(v)->getValue(), 8);
      return;
    case ULONG_VAR:
      out.push_back(KindUint);
      putLE(out, (uint64_t) static_cast<const ULongVar *>(v)->getValue(), 8);
      return;
    case BIT64_VAR:
      out.push_back(KindUint);
      putLE(out, (uint64_t) static_cast<const Bit64Var *>(v)->getValue(), 8);
      return;
    case BIT32_VAR:
      out.push_back(KindBit32);
      putLE(out, (uint64_t) static_cast<const Bit32Var *>(v)->getValue(), 4);
      return;
    case FLOAT_VAR:
    {
      double d = static_cast<const FloatVar *>(v)->getValue();
      uint64_t bits;
      memcpy(&bits, &d, 8);
      out.push_back(KindFloat);
      putLE(out, bits, 8);
      return;
    }
    case TEXT_VAR:
    {
      const CharString &s = static_cast<const TextVar *>(v)->getString();
      out.push_back(KindString);
      out.insert(out.end(), (const uint8_t *)(const char *)s, (const uint8_t *)(const char *)s + s.len());
      return;
    }
    case LANGTEXT_VAR:
    {
      const CharString &s = static_cast<const LangTextVar *>(v)->getValue().getText();
      out.push_back(KindLangText);
      out.insert(out.end(), (const uint8_t *)(const char *)s, (const uint8_t *)(const char *)s + s.len());
      return;
    }
    case TIME_VAR:
    {
      const TimeVar *t = static_cast<const TimeVar *>(v);
      int64_t ms = (int64_t)t->getSeconds() * 1000 + t->getMilli();
      out.push_back(KindTime);
      putLE(out, (uint64_t)ms, 8);
      return;
    }
    case BLOB_VAR:
    {
      const Blob &b = static_cast<const BlobVar *>(v)->getValue();
      out.push_back(KindBytes);
      if (b.getLen())
        out.insert(out.end(), b.getData(), b.getData() + b.getLen());
      return;
    }
    case DPIDENTIFIER_VAR:
    {
      CharString name;
      Manager::getName(static_cast<const DpIdentifierVar *>(v)->getValue(), name);
      out.push_back(KindString);
      out.insert(out.end(), (const uint8_t *)(const char *)name, (const uint8_t *)(const char *)name + name.len());
      return;
    }
    case ANYTYPE_VAR:
      encodeInto(out, static_cast<const AnyTypeVar *>(v)->getVar());
      return;
    default:
      break;
  }
  if (v->isDynVar())
  {
    const DynVar *d = static_cast<const DynVar *>(v);
    Writer items;
    for (unsigned int i = 0; i < d->getArrayLength(); i++)
      items.value(TagValue, d->getAt(i));
    out.push_back(KindDyn);
    if (!items.empty())
      out.insert(out.end(), items.data(), items.data() + items.size());
    return;
  }
  out.push_back(KindNull);
}

bool Message::parse(const uint8_t *p, uint32_t n)
{
  list.clear();
  uint32_t off = 0;
  while (off < n)
  {
    if (n - off < 5)
      return false;
    Field f;
    f.tag = p[off];
    f.n = (uint32_t)getLE(p + off + 1, 4);
    off += 5;
    if (f.n > n - off)
      return false;
    f.p = p + off;
    off += f.n;
    list.push_back(f);
  }
  return true;
}

const Field *Message::first(uint8_t tag) const
{
  for (const Field &f : list)
    if (f.tag == tag)
      return &f;
  return nullptr;
}

std::vector<const Field *> Message::all(uint8_t tag) const
{
  std::vector<const Field *> out;
  for (const Field &f : list)
    if (f.tag == tag)
      out.push_back(&f);
  return out;
}

std::string fieldString(const Field *f)
{
  return f ? std::string((const char *)f->p, f->n) : std::string();
}

std::string Message::str(uint8_t tag) const { return fieldString(first(tag)); }

bool Message::u32(uint8_t tag, uint32_t &out) const
{
  const Field *f = first(tag);
  if (!f || f->n != 4)
    return false;
  out = (uint32_t)getLE(f->p, 4);
  return true;
}

bool Message::u64(uint8_t tag, uint64_t &out) const
{
  const Field *f = first(tag);
  if (!f || f->n != 8)
    return false;
  out = getLE(f->p, 8);
  return true;
}

uint32_t kindOfElement(DpElementType et)
{
  switch (et)
  {
    case DPELEMENT_RECORD:
    case DPELEMENT_TYPEREFERENCE:
      return ElemStruct;
    case DPELEMENT_BIT:
      return KindBool;
    case DPELEMENT_INT:
    case DPELEMENT_CHAR:
    case DPELEMENT_LONG:
      return KindInt;
    case DPELEMENT_UINT:
    case DPELEMENT_ULONG:
    case DPELEMENT_64BIT:
      return KindUint;
    case DPELEMENT_32BIT:
      return KindBit32;
    case DPELEMENT_FLOAT:
      return KindFloat;
    case DPELEMENT_TEXT:
      return KindString;
    case DPELEMENT_TIME:
      return KindTime;
    case DPELEMENT_BLOB:
      return KindBytes;
    case DPELEMENT_LANGTEXT:
      return KindLangText;
    case DPELEMENT_DYNCHAR:
    case DPELEMENT_DYNUINT:
    case DPELEMENT_DYNINT:
    case DPELEMENT_DYNFLOAT:
    case DPELEMENT_DYNBIT:
    case DPELEMENT_DYN32BIT:
    case DPELEMENT_DYNTEXT:
    case DPELEMENT_DYNTIME:
    case DPELEMENT_DYNLANGTEXT:
    case DPELEMENT_DYNBLOB:
    case DPELEMENT_DYNLONG:
    case DPELEMENT_DYNULONG:
    case DPELEMENT_DYN64BIT:
      return KindDyn;
    default:
      return ElemUnsupported;
  }
}

Variable *decodeValue(const Field &f, DpElementType et, std::string &err)
{
  if (f.n < 1)
  {
    err = "empty value";
    return nullptr;
  }
  uint8_t kind = f.p[0];
  const uint8_t *b = f.p + 1;
  uint32_t n = f.n - 1;
  uint32_t want = kindOfElement(et);
  if (want != kind || want == ElemStruct || want == ElemUnsupported || want == KindDyn || want == KindLangText)
  {
    err = "value kind " + std::to_string(kind) + " does not match element kind " + std::to_string(want);
    return nullptr;
  }
  switch (et)
  {
    case DPELEMENT_BIT:
      if (n != 1) break;
      return new BitVar(b[0] ? PVSS_TRUE : PVSS_FALSE);
    case DPELEMENT_INT:
      if (n != 8) break;
      return new IntegerVar((PVSSlong)(int64_t)getLE(b, 8));
    case DPELEMENT_CHAR:
      if (n != 8) break;
      return new CharVar((PVSSuchar)getLE(b, 8));
    case DPELEMENT_LONG:
      if (n != 8) break;
      return new LongVar((PVSSlonglong)(int64_t)getLE(b, 8));
    case DPELEMENT_UINT:
      if (n != 8) break;
      return new UIntegerVar((PVSSulong)getLE(b, 8));
    case DPELEMENT_ULONG:
    case DPELEMENT_64BIT:
      if (n != 8) break;
      return new ULongVar((PVSSulonglong)getLE(b, 8));
    case DPELEMENT_32BIT:
      if (n != 4) break;
      return new Bit32Var(Bit32((PVSSulong)getLE(b, 4)));
    case DPELEMENT_FLOAT:
    {
      if (n != 8) break;
      uint64_t bits = getLE(b, 8);
      double d;
      memcpy(&d, &bits, 8);
      return new FloatVar(d);
    }
    case DPELEMENT_TEXT:
      return new TextVar((const char *)b, (size_t)n);
    case DPELEMENT_TIME:
    {
      if (n != 8) break;
      int64_t ms = (int64_t)getLE(b, 8);
      int64_t sec = ms / 1000, milli = ms % 1000;
      if (milli < 0)
      {
        milli += 1000;
        sec -= 1;
      }
      return new TimeVar((time_t)sec, (PVSSshort)milli);
    }
    case DPELEMENT_BLOB:
      return new BlobVar(const_cast<PVSSuchar *>(b), (PVSSulong)n, true);
    default:
      break;
  }
  err = "malformed value body";
  return nullptr;
}

bool encodeTable(Writer &w, const Variable *v)
{
  if (v && v->isA() == ANYTYPE_VAR)
    v = static_cast<const AnyTypeVar *>(v)->getVar();
  if (!v || !v->isDynVar())
    return false;
  const DynVar *rows = static_cast<const DynVar *>(v);
  for (unsigned int r = 0; r < rows->getArrayLength(); r++)
  {
    const Variable *row = rows->getAt(r);
    if (row && row->isA() == ANYTYPE_VAR)
      row = static_cast<const AnyTypeVar *>(row)->getVar();
    Writer cells;
    if (row && row->isDynVar())
    {
      const DynVar *d = static_cast<const DynVar *>(row);
      for (unsigned int c = 0; c < d->getArrayLength(); c++)
        cells.value(TagValue, d->getAt(c));
    }
    w.nested(TagRow, cells);
  }
  return true;
}

}  // namespace mmq

namespace mmq
{

bool encodeTableChunks(const Variable *v, uint32_t maxBytes, const std::function<void(const Writer &, bool)> &sink)
{
  if (v && v->isA() == ANYTYPE_VAR)
    v = static_cast<const AnyTypeVar *>(v)->getVar();
  if (!v || !v->isDynVar())
    return false;
  const DynVar *rows = static_cast<const DynVar *>(v);
  auto rowWriter = [](const Variable *row) {
    if (row && row->isA() == ANYTYPE_VAR)
      row = static_cast<const AnyTypeVar *>(row)->getVar();
    Writer cells;
    if (row && row->isDynVar())
    {
      const DynVar *d = static_cast<const DynVar *>(row);
      for (unsigned int c = 0; c < d->getArrayLength(); c++)
        cells.value(TagValue, d->getAt(c));
    }
    return cells;
  };
  unsigned int n = rows->getArrayLength();
  if (n == 0)
  {
    sink(Writer(), true);
    return true;
  }
  Writer header = rowWriter(rows->getAt(0));
  Writer chunk;
  chunk.nested(TagRow, header);
  bool hasRows = false;
  for (unsigned int r = 1; r < n; r++)
  {
    Writer cells = rowWriter(rows->getAt(r));
    if (hasRows && chunk.size() + cells.size() + 5 > maxBytes)
    {
      sink(chunk, false);
      chunk = Writer();
      chunk.nested(TagRow, header);
      hasRows = false;
    }
    chunk.nested(TagRow, cells);
    hasRows = true;
  }
  sink(chunk, true);
  return true;
}

}  // namespace mmq
