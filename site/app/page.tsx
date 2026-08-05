import Nav from "../components/Nav";
import Hero from "../components/Hero";
import Problem from "../components/Problem";
import Pillars from "../components/Pillars";
import Architecture from "../components/Architecture";
import Streaming from "../components/Streaming";
import Quickstart from "../components/Quickstart";
import Footer from "../components/Footer";

export default function Home() {
  return (
    <main>
      <Nav />
      <Hero />
      <Problem />
      <Pillars />
      <Architecture />
      <Streaming />
      <Quickstart />
      <Footer />
    </main>
  );
}
