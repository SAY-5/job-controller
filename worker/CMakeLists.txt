cmake_minimum_required(VERSION 3.20)
project(jobworker CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra -Wpedantic -Wshadow -Wnon-virtual-dtor -Werror)

add_library(jobworker_core STATIC
  src/compute.cpp
  src/checkpoint.cpp
)
target_include_directories(jobworker_core PUBLIC src)
target_compile_features(jobworker_core PUBLIC cxx_std_20)

add_executable(jobworker src/main.cpp)
target_link_libraries(jobworker PRIVATE jobworker_core)
target_compile_features(jobworker PRIVATE cxx_std_20)

option(BUILD_WORKER_TESTS "Build worker GoogleTest suite" ON)
if(BUILD_WORKER_TESTS)
  include(FetchContent)
  FetchContent_Declare(
    googletest
    GIT_REPOSITORY https://github.com/google/googletest.git
    GIT_TAG v1.14.0
  )
  set(gtest_force_shared_crt ON CACHE BOOL "" FORCE)
  FetchContent_MakeAvailable(googletest)

  enable_testing()
  add_executable(jobworker_tests
    tests/test_compute.cpp
    tests/test_checkpoint.cpp
  )
  target_link_libraries(jobworker_tests PRIVATE jobworker_core GTest::gtest_main)
  target_compile_features(jobworker_tests PRIVATE cxx_std_20)

  include(GoogleTest)
  gtest_discover_tests(jobworker_tests)
endif()
