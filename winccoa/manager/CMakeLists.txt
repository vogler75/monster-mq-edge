# WinCC OA API manager embedding the MonsterMQ Edge broker.
#
# Build (developer-run, see README.md):
#   export API_ROOT=/opt/WinCC_OA/3.21/api
#   make -C ../.. embed-lib            # builds ../../build/embed/libmonstermq.a
#   mkdir build && cd build && cmake .. && make
cmake_minimum_required(VERSION 3.21.1)

set(API_ROOT_ENV "$ENV{API_ROOT}" CACHE FILEPATH "directory of the WinCC_OA API installation")
string(REPLACE "\"" "" API_ROOT "${API_ROOT_ENV}")

include(${API_ROOT}/CMakeDefines.txt)

project(monstermq)

set(TARGET WCCOAmmq)

set(MMQ_EMBED_DIR "${CMAKE_CURRENT_SOURCE_DIR}/../../build/embed" CACHE PATH "directory with libmonstermq.a and monstermq.h")

set(SOURCES
  MonsterMQMain.cxx
  MonsterMQManager.cxx
  MonsterMQProbe.cxx
  MonsterMQResources.cxx
  MmqTlv.cxx
)

add_manager(${TARGET} ${SOURCES})
target_include_directories(${TARGET} PRIVATE ${MMQ_EMBED_DIR})
# The Go c-archive needs pthread and dl; it is linked last.
target_link_libraries(${TARGET} ${MMQ_EMBED_DIR}/libmonstermq.a pthread dl)
