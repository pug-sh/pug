'use client'

import {
  GalleryVerticalEnd,
  Map,
  PieChart,
} from 'lucide-react'
import * as React from 'react'
import { useAtom } from 'jotai'
import { useLocation } from 'wouter'
import { batchGetAtom } from '@/atoms/projects'

import { ProjectSwitcher } from '@/components/nav/project-switcher'
import { NavUser } from '@/components/nav/nav-user'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuItem,
  SidebarMenuButton,
  SidebarRail,
} from '@/components/ui/sidebar'

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const [projects] = useAtom(batchGetAtom)
  const [location] = useLocation()

  // Convert projects to the format expected by ProjectSwitcher
  const projectSwitcherProjects = React.useMemo(() => {
    return projects.map(project => ({
      name: project.displayName,
      logo: GalleryVerticalEnd, // Default icon for all projects
      plan: 'Enterprise', // Placeholder - would need to come from project data if available in the API
    }))
  }, [projects])

  // Helper function to check if a route is active
  const isActive = (route: string) => location === route

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <ProjectSwitcher projects={projectSwitcherProjects} />
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/campaigns')}>
                <a href="/campaigns">
                  <PieChart />
                  <span>Campaigns</span>
                </a>
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/journeys')}>
                <a href="/journeys">
                  <Map />
                  <span>Journeys</span>
                </a>
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/projects')}>
                <a href="/projects">
                  <GalleryVerticalEnd />
                  <span>Projects</span>
                </a>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
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
