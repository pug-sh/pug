'use client'

import {
  BookOpen,
  Bot,
  Frame,
  GalleryVerticalEnd,
  Map,
  PieChart,
  Settings2,
  SquareTerminal,
} from 'lucide-react'
import * as React from 'react'
import { useAtom } from 'jotai'
import { batchGetAtom } from '@/atoms/projects'

import { NavMain } from '@/components/nav/nav-main'
import { NavProjects } from '@/components/nav/nav-projects'
import { NavUser } from '@/components/nav/nav-user'
import { ProjectSwitcher } from '@/components/nav/project-switcher'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarRail,
} from '@/components/ui/sidebar'

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const [projects] = useAtom(batchGetAtom)

  // Convert projects to the format expected by ProjectSwitcher
  const projectSwitcherProjects = React.useMemo(() => {
    return projects.map(project => ({
      name: project.displayName,
      logo: GalleryVerticalEnd, // Default icon for all projects
      plan: 'Enterprise', // Placeholder - would need to come from project data if available in the API
    }))
  }, [projects])

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <ProjectSwitcher projects={projectSwitcherProjects} />
      </SidebarHeader>
      <SidebarContent>
        <NavMain items={[
          {
            title: 'Playground',
            url: '#',
            icon: SquareTerminal,
            isActive: true,
            items: [
              {
                title: 'History',
                url: '#',
              },
              {
                title: 'Starred',
                url: '#',
              },
              {
                title: 'Settings',
                url: '#',
              },
            ],
          },
          {
            title: 'Models',
            url: '#',
            icon: Bot,
            items: [
              {
                title: 'Genesis',
                url: '#',
              },
              {
                title: 'Explorer',
                url: '#',
              },
              {
                title: 'Quantum',
                url: '#',
              },
            ],
          },
          {
            title: 'Documentation',
            url: '#',
            icon: BookOpen,
            items: [
              {
                title: 'Introduction',
                url: '#',
              },
              {
                title: 'Get Started',
                url: '#',
              },
              {
                title: 'Tutorials',
                url: '#',
              },
              {
                title: 'Changelog',
                url: '#',
              },
            ],
          },
          {
            title: 'Settings',
            url: '#',
            icon: Settings2,
            items: [
              {
                title: 'General',
                url: '#',
              },
              {
                title: 'Team',
                url: '#',
              },
              {
                title: 'Billing',
                url: '#',
              },
              {
                title: 'Limits',
                url: '#',
              },
            ],
          },
        ]} />
        <NavProjects projects={[
          {
            name: 'Design Engineering',
            url: '#',
            icon: Frame,
          },
          {
            name: 'Sales & Marketing',
            url: '#',
            icon: PieChart,
          },
          {
            name: 'Travel',
            url: '#',
            icon: Map,
          },
        ]} />
      </SidebarContent>
      <SidebarFooter>
        <NavUser user={{
          name: 'shadcn',
          email: 'm@example.com',
          avatar: '/avatars/shadcn.jpg',
        }} />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
