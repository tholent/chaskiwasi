# Host test tier for the Chaski firmware.
#
# This builds the firmware's logic components on the development machine, with
# no ESP-IDF and no hardware, and runs GoogleTest against them. It is possible
# only because those components include no esp_* and no FreeRTOS headers
# (chaski-implementation-plan ground rule 3) — hardware reaches them through
# seams, and the seams have host implementations.
#
# What lives here: everything assertable off-hardware — the sync engine's
# ordering, dedup, ack handling, grapheme line-breaking, the wipe SEQUENCE
# against a recording panel, token verification, the strings boundary.
# What does not: anything needing the panel, the modem, or a battery. Those are
# the bench tier (test/firmware/bench) and the gates.
#
# Build and run:
#   cmake -S test/firmware/host -B build/hosttest -G Ninja
#   cmake --build build/hosttest && ctest --test-dir build/hosttest
cmake_minimum_required(VERSION 3.16)
project(chaski_host_tests CXX C)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Debug)
endif()

add_compile_options(-Wall -Wextra -Wpedantic)

find_package(GTest REQUIRED)
include(GoogleTest)
enable_testing()

# Third-party dependencies, obtained differently on each side but arranged so
# component sources are identical: cJSON comes from the system package here and
# from IDF's `json` component on the target, while utf8proc is compiled from
# vendored source on BOTH (see below).
find_path(CJSON_INCLUDE_DIR cjson/cJSON.h REQUIRED)
find_library(CJSON_LIB cjson REQUIRED)
add_library(cjson INTERFACE)
# Both spellings are exposed on purpose. Debian ships the header as
# <cjson/cJSON.h>; ESP-IDF's `json` component exposes it top-level as "cJSON.h".
# Component code uses the IDF spelling — it is the one that must work on the
# device — and adding the cjson/ subdirectory here makes that spelling resolve
# on the host too, so no source file needs an #ifdef to be portable. (F-C3:
# the target build caught this; the host tier could not have.)
target_include_directories(cjson INTERFACE
                           ${CJSON_INCLUDE_DIR} ${CJSON_INCLUDE_DIR}/cjson)
target_link_libraries(cjson INTERFACE ${CJSON_LIB})

set(CHASKI_COMPONENTS ${CMAKE_CURRENT_LIST_DIR}/../../../firmware/chaski/components)

# utf8proc is built from the vendored source, NOT from the system package.
# ESP-IDF ships no utf8proc and the component registry has no port, so the
# device compiles firmware/chaski/components/utf8proc; if the host linked
# Debian's build instead, C-9 would validate one implementation while the
# device ran another — the exact skew B.7 exists to prevent (F-C8).
add_subdirectory(${CHASKI_COMPONENTS}/utf8proc ${CMAKE_BINARY_DIR}/components/utf8proc)


# Every component except modem, which is target-only by design: it wraps the
# vendor library, and every decision worth testing lives above the transport
# seam.
foreach(c wire store ayllu layout transport syncengine panel input power pututu kipu ui)
  add_subdirectory(${CHASKI_COMPONENTS}/${c} ${CMAKE_BINARY_DIR}/components/${c})
endforeach()

# The strings table is C and lives in main/; tests need it to assert the
# vocabulary boundary and that no id is missing text (C-15).
add_library(chaski_strings STATIC
            ${CMAKE_CURRENT_LIST_DIR}/../../../firmware/chaski/main/chaski_strings.c)
target_include_directories(chaski_strings PUBLIC
                           ${CMAKE_CURRENT_LIST_DIR}/../../../firmware/chaski/main)

# Path to generated wire fixtures, so tests can find testdata regardless of
# where the build tree lives.
add_compile_definitions(
  CHASKI_TESTDATA_DIR="${CMAKE_CURRENT_LIST_DIR}/testdata")

# chaski_add_test(<name> <source> [libs...])
#
# Test names carry their C-number where they implement one, so the client
# spec's §15 table maps to this suite mechanically (ground rule 6).
function(chaski_add_test name src)
  add_executable(${name} ${src})
  target_link_libraries(${name} PRIVATE GTest::gtest GTest::gtest_main ${ARGN})
  gtest_discover_tests(${name})
endfunction()

add_subdirectory(wire)
add_subdirectory(store)
add_subdirectory(ayllu)
add_subdirectory(layout)
add_subdirectory(syncengine)
add_subdirectory(transport)
add_subdirectory(panel)
add_subdirectory(input)
add_subdirectory(power)
add_subdirectory(pututu)
add_subdirectory(kipu)
add_subdirectory(strings)
add_subdirectory(draft)
add_subdirectory(wake)
