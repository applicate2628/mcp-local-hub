cmake_minimum_required(VERSION 3.20)
project(hello LANGUAGES CXX)

# A target that compiles and runs cleanly (built by the default "all" target).
add_executable(hello hello.cpp)

# A target that FAILS to compile. EXCLUDE_FROM_ALL keeps it out of the default
# build so `cmake --build` succeeds; the integration test builds it explicitly
# to exercise the structured-diagnostics path.
add_executable(broken EXCLUDE_FROM_ALL broken.cpp)

enable_testing()

# One passing test (runs the hello executable) and one deliberately-failing
# test (`cmake -E false` always exits non-zero) — no compiler needed for the
# failing case, so ctest deterministically reports 1 passed + 1 failed.
add_test(NAME hello_pass COMMAND hello)
add_test(NAME hello_fail COMMAND ${CMAKE_COMMAND} -E false)
