int main() {
    // Deliberate compile error: use of an undeclared identifier. Produces a
    // parseable diagnostic in every supported compiler format (MSVC C2065,
    // GCC/Clang "use of undeclared identifier").
    return this_symbol_does_not_exist;
}
